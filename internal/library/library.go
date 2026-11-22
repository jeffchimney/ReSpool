// Package library scans a media folder and groups its audio files into
// feeds: every folder of audio files becomes one feed, and every audio file
// sitting directly in the media root becomes a feed of its own.
package library

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeffchimney/respool/internal/media"
)

// Library holds the most recent scan of its folders. It is safe for
// concurrent use.
type Library struct {
	roots  []Root
	logger *slog.Logger

	snap     atomic.Pointer[Snapshot]
	scanning atomic.Bool
	pending  atomic.Bool // a rescan was requested but hasn't started
	rescan   chan struct{}

	scanMu sync.Mutex
	cache  map[string]cachedInfo // by relative path; guarded by scanMu
}

type cachedInfo struct {
	size int64
	mod  time.Time
	info media.Info
}

// Snapshot is an immutable view of the library at the end of a scan.
type Snapshot struct {
	Feeds     []*Feed
	ScannedAt time.Time
	byID      map[string]*Feed
}

// Feed returns the feed with the given ID, or nil.
func (s *Snapshot) Feed(id string) *Feed {
	if s == nil {
		return nil
	}
	return s.byID[id]
}

// Feed is one podcast feed: usually a single audiobook.
type Feed struct {
	ID          string
	Path        string // relative to its root, after the root's prefix
	Dir         string // absolute folder holding the feed's files
	Title       string
	Author      string
	Description string
	Cover       Cover
	Episodes    []*Episode // in listening order
	Updated     time.Time
	Duration    time.Duration // sum of the known episode durations

	byID map[string]*Episode
}

// Episode returns the episode with the given ID, or nil.
func (f *Feed) Episode(id string) *Episode { return f.byID[id] }

// Cover says where a feed's artwork comes from.
type Cover struct {
	Path     string // an image file, or an audio file with embedded art; empty when there is none
	Embedded bool
	MIMEType string
	ModTime  time.Time
}

// Ext is the file extension to use in the cover's URL.
func (c Cover) Ext() string {
	if c.Path != "" && c.MIMEType == "image/jpeg" {
		return ".jpg"
	}
	return ".png" // PNG files and generated placeholders
}

// Episode is one audio file in a feed.
type Episode struct {
	ID          string
	Number      int // 1-based position in listening order
	Title       string
	Description string
	Path        string // absolute
	RelPath     string
	Ext         string
	MIMEType    string
	Size        int64
	ModTime     time.Time
	PubDate     time.Time
	Duration    time.Duration
	Chapters    []media.Chapter
}

// Root is a folder the library scans.
type Root struct {
	Dir string
	// Prefix is put in front of the paths of this root's feeds and files so
	// feeds from different roots can't collide. Feed and episode IDs derive
	// from those paths, so leave it empty for the main media folder and
	// never change it once feeds have been shared. It must end in "/".
	Prefix string
}

// New returns a library for the given folders. Call Run or Scan to fill it.
func New(logger *slog.Logger, roots ...Root) *Library {
	return &Library{
		roots:  roots,
		logger: logger,
		rescan: make(chan struct{}, 1),
		cache:  map[string]cachedInfo{},
	}
}

// Snapshot returns the latest scan, or nil before the first scan finishes.
func (l *Library) Snapshot() *Snapshot { return l.snap.Load() }

// Scanning reports whether a scan is in progress or queued.
func (l *Library) Scanning() bool { return l.scanning.Load() || l.pending.Load() }

// Rescan asks Run to scan again as soon as possible.
func (l *Library) Rescan() {
	l.pending.Store(true)
	select {
	case l.rescan <- struct{}{}:
	default: // one is already queued
	}
}

// Run scans immediately, then again every interval (if positive) and
// whenever Rescan is called, until ctx is done.
func (l *Library) Run(ctx context.Context, interval time.Duration) {
	l.scanAndLog()
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-l.rescan:
		}
		l.scanAndLog()
	}
}

func (l *Library) scanAndLog() {
	start := time.Now()
	snap, err := l.Scan()
	if err != nil {
		l.logger.Error("library scan failed", "err", err)
		return
	}
	episodes := 0
	for _, f := range snap.Feeds {
		episodes += len(f.Episodes)
	}
	l.logger.Info("library scanned", "feeds", len(snap.Feeds), "episodes", episodes,
		"took", time.Since(start).Round(time.Millisecond))
}

type file struct {
	rel, abs string
	size     int64
	mod      time.Time
	disc     int // from a "CD 2"-style folder; 0 otherwise
}

type group struct {
	key    string // relative path of the feed's folder, or of the file for single-file feeds
	dir    string // absolute folder holding the feed's files
	single bool
	files  []file
}

// Scan walks the media folder, reads metadata for new or changed files and
// publishes a new snapshot.
func (l *Library) Scan() (*Snapshot, error) {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()
	l.scanning.Store(true)
	l.pending.Store(false)
	defer l.scanning.Store(false)

	groups, err := l.walk()
	if err != nil {
		return nil, err
	}

	cache := make(map[string]cachedInfo, len(l.cache))
	var todo []file
	for _, g := range groups {
		for _, f := range g.files {
			if c, ok := l.cache[f.rel]; ok && c.size == f.size && c.mod.Equal(f.mod) {
				cache[f.rel] = c
			} else {
				todo = append(todo, f)
			}
		}
	}
	for i, info := range l.probeAll(todo) {
		f := todo[i]
		cache[f.rel] = cachedInfo{size: f.size, mod: f.mod, info: info}
	}
	l.cache = cache

	snap := &Snapshot{ScannedAt: time.Now(), byID: make(map[string]*Feed, len(groups))}
	for _, g := range groups {
		f := buildFeed(g, cache)
		if other, dup := snap.byID[f.ID]; dup {
			l.logger.Warn("feed ID collision; skipping feed", "id", f.ID, "path", f.Path, "other", other.Path)
			continue
		}
		snap.byID[f.ID] = f
		snap.Feeds = append(snap.Feeds, f)
	}
	sort.Slice(snap.Feeds, func(i, j int) bool {
		a, b := snap.Feeds[i], snap.Feeds[j]
		if !strings.EqualFold(a.Title, b.Title) {
			return naturalLess(a.Title, b.Title)
		}
		return a.Path < b.Path
	})
	l.snap.Store(snap)
	return snap, nil
}

func (l *Library) walk() (map[string]*group, error) {
	groups := map[string]*group{}
	for _, root := range l.roots {
		if err := l.walkRoot(root, groups); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

func (l *Library) walkRoot(root Root, groups map[string]*group) error {
	return filepath.WalkDir(root.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root.Dir {
				return err
			}
			l.logger.Warn("skipping unreadable path", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != root.Dir && skipName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !media.IsAudio(d.Name()) {
			return nil
		}
		info, err := os.Stat(p) // follows symlinked files
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root.Dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		dir, disc := path.Dir(rel), 0
		if dir != "." {
			if n, ok := discNumber(path.Base(dir)); ok {
				dir, disc = path.Dir(dir), n
			}
		}
		key, single := dir, false
		if dir == "." {
			key, single = rel, true
		}
		key = root.Prefix + key
		g := groups[key]
		if g == nil {
			g = &group{key: key, dir: filepath.Join(root.Dir, filepath.FromSlash(dir)), single: single}
			groups[key] = g
		}
		g.files = append(g.files, file{rel: root.Prefix + rel, abs: p, size: info.Size(), mod: info.ModTime(), disc: disc})
		return nil
	})
}

func (l *Library) probeAll(files []file) []media.Info {
	out := make([]media.Info, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), 8) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				info, err := media.Probe(files[i].abs)
				if err != nil {
					l.logger.Warn("could not read all metadata", "file", files[i].rel, "err", err)
				}
				out[i] = info
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

func buildFeed(g *group, cache map[string]cachedInfo) *Feed {
	info := func(f file) media.Info { return cache[f.rel].info }
	files := g.files

	// Track tags decide the order only when every file has a distinct one;
	// otherwise natural filename order ("2" before "10") is more reliable.
	useTags := len(files) > 1
	seen := map[[2]int]bool{}
	for _, f := range files {
		in := info(f)
		k := [2]int{max(f.disc, in.Disc), in.Track}
		if in.Track <= 0 || seen[k] {
			useTags = false
			break
		}
		seen[k] = true
	}
	disc := func(f file) int {
		if f.disc > 0 || !useTags {
			return f.disc
		}
		return info(f).Disc
	}
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if da, db := disc(a), disc(b); da != db {
			return da < db
		}
		if ta, tb := info(a).Track, info(b).Track; useTags && ta != tb {
			return ta < tb
		}
		return naturalLess(a.rel, b.rel)
	})

	feed := &Feed{Path: g.key, Dir: g.dir, byID: make(map[string]*Episode, len(files))}
	name := path.Base(g.key)
	if g.single {
		name = stem(name)
	}
	feed.ID = feedID(g.key, name)

	first := info(files[0])
	if g.single {
		feed.Title = firstNonEmpty(first.Album, first.Title, name)
	} else {
		feed.Title = firstNonEmpty(majority(files, func(f file) string { return info(f).Album }), name)
	}
	for _, f := range files {
		in := info(f)
		if feed.Author = firstNonEmpty(in.AlbumArtist, in.Artist); feed.Author != "" {
			break
		}
	}

	// Taggers often give every file of a book the book's title; numbered
	// filenames are more useful then.
	sameTitles := len(files) > 1
	for _, f := range files {
		if t := info(f).Title; t == "" || t != first.Title {
			sameTitles = false
			break
		}
	}

	for i, f := range files {
		in := info(f)
		ext := strings.ToLower(path.Ext(f.rel))
		mime, _ := media.MIMEType(ext)
		e := &Episode{
			ID:       episodeID(f.rel),
			Number:   i + 1,
			Title:    in.Title,
			Path:     f.abs,
			RelPath:  f.rel,
			Ext:      ext,
			MIMEType: mime,
			Size:     f.size,
			ModTime:  f.mod,
			Duration: in.Duration,
			Chapters: in.Chapters,
		}
		if e.Title == "" || sameTitles {
			e.Title = stem(path.Base(f.rel))
		}
		feed.Episodes = append(feed.Episodes, e)
		feed.byID[e.ID] = e
		feed.Duration += in.Duration
		if f.mod.After(feed.Updated) {
			feed.Updated = f.mod
		}
	}

	// Podcast apps order episodes by date, so give them dates one minute
	// apart in listening order, counting back from the newest file. Counting
	// back keeps every date in the past: some apps hide future episodes.
	last := feed.Updated
	if now := time.Now(); last.After(now) {
		last = now
	}
	last = last.UTC().Truncate(time.Minute)
	for i, e := range feed.Episodes {
		e.PubDate = last.Add(-time.Duration(len(feed.Episodes)-1-i) * time.Minute)
	}

	feed.Description = sidecarText(g)
	if feed.Description == "" {
		for _, f := range files {
			if in := info(f); firstNonEmpty(in.Description, in.Comment) != "" {
				feed.Description = firstNonEmpty(in.Description, in.Comment)
				break
			}
		}
	}
	if !g.single {
		o := ReadOverrides(g.dir)
		feed.Title = firstNonEmpty(o.Title, feed.Title)
		feed.Author = firstNonEmpty(o.Author, feed.Author)
		feed.Description = firstNonEmpty(o.Description, feed.Description)
	}
	if len(feed.Episodes) == 1 {
		feed.Episodes[0].Description = feed.Description
	}

	feed.Cover = findCover(g)
	if feed.Cover.Path == "" {
		for _, f := range files {
			if in := info(f); in.HasPicture {
				feed.Cover = Cover{Path: f.abs, Embedded: true, MIMEType: in.PictureMIME, ModTime: f.mod}
				break
			}
		}
	}
	if feed.Cover.ModTime.After(feed.Updated) {
		feed.Updated = feed.Cover.ModTime
	}
	return feed
}

var coverNames = []string{"cover", "folder", "front", "poster"}

// findCover looks for artwork next to the feed's files: cover.jpg,
// folder.png and friends, or any lone image in a book's folder. For a single
// file "Book.m4b" it looks for "Book.jpg" or "Book.png".
func findCover(g *group) Cover {
	imageExts := []string{".jpg", ".jpeg", ".png", ".JPG", ".JPEG", ".PNG"}
	if g.single {
		// Stat a few names rather than listing what may be a large root folder.
		base := filepath.Join(g.dir, stem(path.Base(g.key)))
		for _, ext := range imageExts {
			if c, ok := coverAt(base + ext); ok {
				return c
			}
		}
		return Cover{}
	}

	entries, err := os.ReadDir(g.dir)
	if err != nil {
		return Cover{}
	}
	images := map[string]string{} // lower-cased name → real name
	var names []string
	for _, e := range entries {
		ext := strings.ToLower(path.Ext(e.Name()))
		if e.IsDir() || skipName(e.Name()) || (ext != ".jpg" && ext != ".jpeg" && ext != ".png") {
			continue
		}
		images[strings.ToLower(e.Name())] = e.Name()
		names = append(names, strings.ToLower(e.Name()))
	}
	for _, base := range coverNames {
		for _, ext := range imageExts[:3] {
			if name, ok := images[base+ext]; ok {
				if c, ok := coverAt(filepath.Join(g.dir, name)); ok {
					return c
				}
			}
		}
	}
	if len(names) > 0 {
		sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
		if c, ok := coverAt(filepath.Join(g.dir, images[names[0]])); ok {
			return c
		}
	}
	return Cover{}
}

func coverAt(p string) (Cover, bool) {
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return Cover{}, false
	}
	mime := "image/jpeg"
	if strings.EqualFold(path.Ext(p), ".png") {
		mime = "image/png"
	}
	return Cover{Path: p, MIMEType: mime, ModTime: info.ModTime()}, true
}

// sidecarText reads a book description from description.txt or desc.txt in
// the book's folder, or Book.txt next to a single file.
func sidecarText(g *group) string {
	var candidates []string
	if g.single {
		candidates = []string{filepath.Join(g.dir, stem(path.Base(g.key))+".txt")}
	} else {
		entries, err := os.ReadDir(g.dir)
		if err != nil {
			return ""
		}
		for _, want := range []string{"description.txt", "desc.txt"} {
			for _, e := range entries {
				if !e.IsDir() && strings.EqualFold(e.Name(), want) {
					candidates = append(candidates, filepath.Join(g.dir, e.Name()))
				}
			}
		}
	}
	for _, p := range candidates {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
		f.Close()
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return ""
}

// majority returns the value shared by at least half of the files, or "".
func majority(files []file, value func(file) string) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, f := range files {
		v := value(f)
		if v == "" {
			continue
		}
		counts[v]++
		if counts[v] > bestN {
			best, bestN = v, counts[v]
		}
	}
	if bestN*2 >= len(files) {
		return best
	}
	return ""
}

func stem(name string) string { return strings.TrimSuffix(name, path.Ext(name)) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
