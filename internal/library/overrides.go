package library

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OverridesFile, in a book's folder, sets details that take precedence over
// tags and folder names. Uploads write it when someone types a title or
// author; people can also create one by hand.
const OverridesFile = "respool.json"

// Overrides are the details OverridesFile can set. Empty fields are left to
// the tags and folder names.
type Overrides struct {
	Title       string `json:"title,omitempty"`
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
}

// ReadOverrides reads OverridesFile from dir. A missing or malformed file
// gives empty Overrides.
func ReadOverrides(dir string) Overrides {
	var o Overrides
	f, err := os.Open(filepath.Join(dir, OverridesFile))
	if err != nil {
		return o
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil || json.Unmarshal(b, &o) != nil {
		return Overrides{}
	}
	o.Title = strings.TrimSpace(o.Title)
	o.Author = strings.TrimSpace(o.Author)
	o.Description = strings.TrimSpace(o.Description)
	return o
}

// WriteOverrides merges o's non-empty fields into dir's OverridesFile.
func WriteOverrides(dir string, o Overrides) error {
	cur := ReadOverrides(dir)
	cur.Title = firstNonEmpty(o.Title, cur.Title)
	cur.Author = firstNonEmpty(o.Author, cur.Author)
	cur.Description = firstNonEmpty(o.Description, cur.Description)
	return SaveOverrides(dir, cur)
}

// SaveOverrides replaces dir's OverridesFile with o. Empty fields go back to
// the tags; if every field is empty the file is removed.
func SaveOverrides(dir string, o Overrides) error {
	o = Overrides{strings.TrimSpace(o.Title), strings.TrimSpace(o.Author), strings.TrimSpace(o.Description)}
	p := filepath.Join(dir, OverridesFile)
	if o == (Overrides{}) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}
