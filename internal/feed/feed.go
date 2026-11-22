// Package feed renders library feeds as podcast RSS, OPML and Podcasting 2.0
// chapter files.
package feed

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jeffchimney/respool/internal/library"
)

// Links builds the absolute URLs that feeds point at. Base is the server's
// public URL including any token prefix, without a trailing slash.
type Links struct{ Base string }

func (l Links) Index() string { return l.Base + "/" }
func (l Links) OPML() string  { return l.Base + "/library.opml" }

func (l Links) Feed(f *library.Feed) string { return l.Base + "/feeds/" + f.ID + ".xml" }

func (l Links) Audio(f *library.Feed, e *library.Episode) string {
	return l.Base + "/audio/" + f.ID + "/" + e.ID + e.Ext
}

func (l Links) Chapters(f *library.Feed, e *library.Episode) string {
	return l.Base + "/chapters/" + f.ID + "/" + e.ID + ".json"
}

// placeholderVersion names the current design of generated covers. Bump it
// when that design changes, so apps and browsers fetch the new one.
const placeholderVersion = "generated-3"

// Cover's URL changes whenever the artwork does, so apps that cache images
// by URL pick up new covers. The extension is part of the path because some
// apps look at it.
func (l Links) Cover(f *library.Feed) string {
	version := fmt.Sprintf("%x", f.Cover.ModTime.Unix())
	if f.Cover.Path == "" {
		version = placeholderVersion
	}
	return l.Base + "/covers/" + f.ID + "/" + version + f.Cover.Ext()
}

// Options are channel-level settings that don't come from the library.
type Options struct {
	Language  string
	Generator string
}

type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Itunes  string   `xml:"xmlns:itunes,attr"`
	Podcast string   `xml:"xmlns:podcast,attr"`
	Atom    string   `xml:"xmlns:atom,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title          string         `xml:"title"`
	Link           string         `xml:"link"`
	AtomLink       atomLink       `xml:"atom:link"`
	Description    string         `xml:"description"`
	Language       string         `xml:"language,omitempty"`
	Generator      string         `xml:"generator,omitempty"`
	LastBuildDate  string         `xml:"lastBuildDate,omitempty"`
	Image          image          `xml:"image"`
	ItunesAuthor   string         `xml:"itunes:author,omitempty"`
	ItunesImage    itunesImage    `xml:"itunes:image"`
	ItunesCategory itunesCategory `xml:"itunes:category"`
	ItunesType     string         `xml:"itunes:type"`
	ItunesExplicit string         `xml:"itunes:explicit"`
	// Private feeds: keep them out of Apple's directory and stop podcast
	// hosts from importing them.
	ItunesBlock   string `xml:"itunes:block"`
	PodcastLocked string `xml:"podcast:locked"`
	Items         []item `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type image struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type itunesImage struct {
	Href string `xml:"href,attr"`
}

type itunesCategory struct {
	Text string          `xml:"text,attr"`
	Sub  *itunesCategory `xml:"itunes:category,omitempty"`
}

type item struct {
	Title             string           `xml:"title"`
	GUID              guid             `xml:"guid"`
	PubDate           string           `xml:"pubDate"`
	Description       string           `xml:"description,omitempty"`
	Enclosure         enclosure        `xml:"enclosure"`
	ItunesDuration    string           `xml:"itunes:duration,omitempty"`
	ItunesEpisode     int              `xml:"itunes:episode"`
	ItunesEpisodeType string           `xml:"itunes:episodeType"`
	Chapters          *podcastChapters `xml:"podcast:chapters,omitempty"`
}

type guid struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type podcastChapters struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// WriteRSS writes f as a podcast RSS feed. The output only changes when the
// feed does, so it can be used for ETags.
func WriteRSS(w io.Writer, f *library.Feed, links Links, opts Options) error {
	feedURL := links.Feed(f)
	cover := links.Cover(f)
	desc := f.Description
	if desc == "" {
		desc = f.Title
	}
	ch := channel{
		Title:          f.Title,
		Link:           links.Index(),
		AtomLink:       atomLink{Href: feedURL, Rel: "self", Type: "application/rss+xml"},
		Description:    desc,
		Language:       opts.Language,
		Generator:      opts.Generator,
		Image:          image{URL: cover, Title: f.Title, Link: links.Index()},
		ItunesAuthor:   f.Author,
		ItunesImage:    itunesImage{Href: cover},
		ItunesCategory: itunesCategory{Text: "Arts", Sub: &itunesCategory{Text: "Books"}},
		ItunesType:     "serial",
		ItunesExplicit: "false",
		ItunesBlock:    "Yes",
		PodcastLocked:  "yes",
	}
	if !f.Updated.IsZero() {
		ch.LastBuildDate = f.Updated.UTC().Format(time.RFC1123Z)
	}

	// Newest first, as most apps expect.
	n := len(f.Episodes)
	for i := n - 1; i >= 0; i-- {
		e := f.Episodes[i]
		it := item{
			Title:             e.Title,
			GUID:              guid{IsPermaLink: "false", Value: "respool:" + f.ID + ":" + e.ID},
			PubDate:           e.PubDate.UTC().Format(time.RFC1123Z),
			Description:       e.Description,
			Enclosure:         enclosure{URL: links.Audio(f, e), Length: e.Size, Type: e.MIMEType},
			ItunesEpisode:     e.Number,
			ItunesEpisodeType: "full",
		}
		if it.Description == "" && n > 1 {
			it.Description = fmt.Sprintf("Part %d of %d of %s.", e.Number, n, f.Title)
		}
		if e.Duration > 0 {
			it.ItunesDuration = strconv.Itoa(int(e.Duration.Round(time.Second).Seconds()))
		}
		if len(e.Chapters) > 0 {
			it.Chapters = &podcastChapters{URL: links.Chapters(f, e), Type: "application/json+chapters"}
		}
		ch.Items = append(ch.Items, it)
	}

	doc := rss{
		Version: "2.0",
		Itunes:  "http://www.itunes.com/dtds/podcast-1.0.dtd",
		Podcast: "https://podcastindex.org/namespace/1.0",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: ch,
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// ChaptersJSON renders an episode's chapters in the Podcasting 2.0 JSON
// chapters format.
func ChaptersJSON(e *library.Episode) ([]byte, error) {
	type chapter struct {
		StartTime float64 `json:"startTime"`
		Title     string  `json:"title"`
	}
	doc := struct {
		Version  string    `json:"version"`
		Chapters []chapter `json:"chapters"`
	}{Version: "1.2.0", Chapters: []chapter{}}
	for _, c := range e.Chapters {
		doc.Chapters = append(doc.Chapters, chapter{StartTime: c.Start.Seconds(), Title: c.Title})
	}
	return json.MarshalIndent(doc, "", "  ")
}

// WriteOPML writes an OPML subscription list of every feed, which many
// podcast apps can import in one go.
func WriteOPML(w io.Writer, feeds []*library.Feed, links Links, title string) error {
	type outline struct {
		Type    string `xml:"type,attr"`
		Text    string `xml:"text,attr"`
		Title   string `xml:"title,attr"`
		XMLURL  string `xml:"xmlUrl,attr"`
		HTMLURL string `xml:"htmlUrl,attr"`
	}
	doc := struct {
		XMLName xml.Name `xml:"opml"`
		Version string   `xml:"version,attr"`
		Head    struct {
			Title string `xml:"title"`
		} `xml:"head"`
		Body struct {
			Outlines []outline `xml:"outline"`
		} `xml:"body"`
	}{Version: "2.0"}
	doc.Head.Title = title
	for _, f := range feeds {
		doc.Body.Outlines = append(doc.Body.Outlines, outline{
			Type: "rss", Text: f.Title, Title: f.Title, XMLURL: links.Feed(f), HTMLURL: links.Index(),
		})
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
