package proxytv

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/csfrancis/proxytv/xmltv"

	log "github.com/sirupsen/logrus"
)

type playlistLoader struct {
	baseAddress string
	filters     []*Filter

	tracks     []Track
	priorities map[string]int
	m3u        strings.Builder
}

func newPlaylistLoader(baseAddress string, filters []*Filter) *playlistLoader {
	return &playlistLoader{
		baseAddress: baseAddress,
		filters:     filters,
		tracks:      make([]Track, 0, len(filters)),
		priorities:  make(map[string]int),
	}
}

func (pl *playlistLoader) findIndexWithID(track *Track) int {
	id := track.Tags["tvg-id"]
	if len(id) == 0 {
		return -1
	}

	for i := range pl.tracks {
		if pl.tracks[i].Tags["tvg-id"] == id {
			return i
		}
	}
	return -1
}

func (pl *playlistLoader) OnPlaylistStart() {
	pl.m3u.Reset()
	pl.m3u.WriteString("#EXTM3U\n")
}

func (pl *playlistLoader) OnTrack(track *Track) {
	if len(pl.filters) == 0 {
		pl.processTrack(track, 0)
		return
	}

	for i, filter := range pl.filters {
		var field string
		switch filter.Type {
		case "id":
			field = "tvg-id"
		case "group":
			field = "group-title"
		case "name":
			field = "tvg-name"
		default:
			log.WithField("type", filter.Type).Panic("invalid filter type")
		}

		val := track.Tags[field]
		if len(val) == 0 {
			continue
		}

		if filter.regexp.Match([]byte(val)) {
			pl.processTrack(track, i)
		}
	}
}

func (pl *playlistLoader) processTrack(track *Track, priority int) {
	name := track.Name

	if len(track.Tags["tvg-id"]) == 0 {
		log.WithField("track", track).Debug("missing tvg-id")
	}

	if existingPriority, exists := pl.priorities[name]; !exists || priority < existingPriority {
		idx := pl.findIndexWithID(track)
		if idx != -1 {
			if strings.Contains(track.Name, "HD") {
				delete(pl.priorities, pl.tracks[idx].Name)
				pl.tracks[idx] = *track
			} else {
				return
			}
		} else {
			if !exists {
				pl.tracks = append(pl.tracks, *track)
			}
		}
		pl.priorities[name] = priority
	} else {
		log.WithField("track", track).Warn("duplicate name")
	}
}

func (pl *playlistLoader) OnPlaylistEnd() {
	sort.SliceStable(pl.tracks, func(i, j int) bool {
		priorityI, existsI := pl.priorities[pl.tracks[i].Name]
		priorityJ, existsJ := pl.priorities[pl.tracks[j].Name]

		if !existsI && !existsJ {
			return false // Keep original order for unmatched elements
		}
		if !existsI {
			return false // Unmatched elements go to the end
		}
		if !existsJ {
			return true // Matched elements come before unmatched ones
		}
		return priorityI < priorityJ
	})

	rewriteURL := len(pl.baseAddress) > 0

	reXuiid := regexp.MustCompile(`xui-id="\{[^"]*\}"\s*`)

	for i := range len(pl.tracks) {
		track := pl.tracks[i]
		uri := track.URI.String()
		if rewriteURL {
			uri = fmt.Sprintf("http://%s/channel/%d", pl.baseAddress, i)
		}
		// Remove xui-id from the tags
		fixedRaw := reXuiid.ReplaceAllString(track.Raw, "")
		pl.m3u.WriteString(fmt.Sprintf("%s\n%s\n", fixedRaw, uri))
	}
}

func loadReader(uri string, userAgent string) (io.ReadCloser, error) {
	var err error
	var reader io.ReadCloser
	logger := log.WithField("uri", uri)
	if isURL(uri) {
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		if err != nil {
			logger.WithError(err).Panic("unable to create request")
		}
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.WithError(err).Panic("unable to load uri")
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("invalid url response code: %d", resp.StatusCode)
		}

		reader = resp.Body
	} else {
		reader, err = os.Open(uri)
		if err != nil {
			return nil, err
		}
	}

	return reader, nil
}

type Provider struct {
	iptvURL     string
	epgURL      string
	baseAddress string
	userAgent   string
	filters     []*Filter

	playlist    *playlistLoader
	epg         *xmltv.TV
	epgData     []byte
	lastRefresh time.Time
}

func NewProvider(config *Config) (*Provider, error) {
	provider := &Provider{
		iptvURL: config.IPTVUrl,
		epgURL:  config.EPGUrl,
		filters: config.Filters,
	}

	if len(config.UserAgent) > 0 {
		provider.userAgent = config.UserAgent
	}

	if config.UseFFMPEG {
		provider.baseAddress = config.ServerAddress
	}

	return provider, nil
}

func (p *Provider) loadXMLTv(reader io.Reader) (*xmltv.TV, error) {
	start := time.Now()

	channels := make(map[string]bool)
	for _, track := range p.playlist.tracks {
		id := track.Tags["tvg-id"]
		if len(id) == 0 {
			continue
		}
		channels[id] = true
	}

	decoder := xml.NewDecoder(reader)
	tvSetup := new(xmltv.TV)

	totalChannelCount := 0
	totalProgrammeCount := 0

	for {
		// Decode the next XML token
		tok, err := decoder.Token()
		if err != nil {
			break // Exit on EOF or error
		}

		// Process the start element
		switch se := tok.(type) {
		case xml.StartElement:
			switch se.Name.Local {
			case "tv":
				for _, attr := range se.Attr {
					switch attr.Name.Local {
					case "date":
						tvSetup.Date = attr.Value
					case "source-info-url":
						tvSetup.SourceInfoURL = attr.Value
					case "source-info-name":
						tvSetup.SourceInfoName = attr.Value
					case "source-data-url":
						tvSetup.SourceDataURL = attr.Value
					case "generator-info-name":
						tvSetup.GeneratorInfoName = attr.Value
					case "generator-info-url":
						tvSetup.GeneratorInfoURL = attr.Value
					}
				}
			case "programme":
				var programme xmltv.Programme
				err := decoder.DecodeElement(&programme, &se)
				if err != nil {
					return nil, err
				}
				if channels[programme.Channel] {
					tvSetup.Programmes = append(tvSetup.Programmes, programme)
				}
				totalProgrammeCount++
			case "channel":
				var channel xmltv.Channel
				err := decoder.DecodeElement(&channel, &se)
				if err != nil {
					return nil, err
				}
				if channels[channel.ID] {
					tvSetup.Channels = append(tvSetup.Channels, channel)
				}
				totalChannelCount++
			}
		}
	}

	log.WithFields(log.Fields{
		"totalChannelCount":   totalChannelCount,
		"channelCount":        len(tvSetup.Channels),
		"totalProgrammeCount": totalProgrammeCount,
		"programmeCount":      len(tvSetup.Programmes),
		"duration":            time.Since(start),
	}).Info("loaded xmltv")

	return tvSetup, nil
}

func (p *Provider) Refresh() error {
	var err error
	log.WithField("url", p.iptvURL).Info("loading IPTV m3u")

	start := time.Now()
	iptvReader, err := loadReader(p.iptvURL, p.userAgent)
	if err != nil {
		return err
	}
	defer iptvReader.Close()
	log.WithField("duration", time.Since(start)).Debug("loaded IPTV m3u")

	pl := newPlaylistLoader(p.baseAddress, p.filters)
	err = loadM3u(iptvReader, pl)
	if err != nil {
		return err
	}
	p.playlist = pl

	log.WithField("channelCount", len(p.playlist.tracks)).Info("parsed IPTV m3u")

	log.WithField("url", p.epgURL).Info("loading EPG")

	start = time.Now()
	epgReader, err := loadReader(p.epgURL, p.userAgent)
	if err != nil {
		return err
	}
	defer epgReader.Close()
	log.WithField("duration", time.Since(start)).Debug("loaded EPG")

	p.epg, err = p.loadXMLTv(epgReader)
	if err != nil {
		return err
	}

	xmlData, err := xml.Marshal(p.epg)
	if err != nil {
		return err
	}

	xmlHeader := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?><!DOCTYPE tv SYSTEM \"xmltv.dtd\">")
	p.epgData = append(xmlHeader, xmlData...)

	p.lastRefresh = time.Now()

	return nil
}

func (p *Provider) GetM3u() string {
	return p.playlist.m3u.String()
}

func (p *Provider) GetEpgXML() string {
	return string(p.epgData)
}

var trackNotFound = Track{}

func (p *Provider) GetTrack(idx int) *Track {
	if idx >= len(p.playlist.tracks) {
		return &trackNotFound
	}
	return &p.playlist.tracks[idx]
}

func (p *Provider) GetLastRefresh() time.Time {
	return p.lastRefresh
}
