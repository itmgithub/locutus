package borg

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"time"
)

// v1 is the driver for borg 1.2 and newer 1.x releases.
type v1 struct{}

const lockWait = "600" // seconds to wait for a repository lock

func (v1) Version() int { return 1 }

func archiveRef(repo, archive string) string { return repo + "::" + archive }

func (v1) InitArgs(repo, encryption string) []string {
	return []string{"init", "--encryption", encryption, "--", repo}
}

func (v1) InfoArgs(repo string) []string {
	return []string{"info", "--lock-wait", lockWait, "--", repo}
}

func (v1) ListArchivesArgs(repo, glob string) []string {
	args := []string{"list", "--json", "--lock-wait", lockWait}
	if glob != "" {
		args = append(args, "--glob-archives", glob)
	}
	return append(args, "--", repo)
}

func parseBorgTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05.999999Z07:00", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (v1) ParseArchives(out []byte) ([]Archive, error) {
	var resp struct {
		Archives []struct {
			Name  string `json:"name"`
			Start string `json:"start"`
		} `json:"archives"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	list := make([]Archive, 0, len(resp.Archives))
	for _, a := range resp.Archives {
		list = append(list, Archive{Name: a.Name, Start: parseBorgTime(a.Start)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Start.After(list[j].Start) })
	return list, nil
}

func (v1) ListItemsArgs(repo, archive string) []string {
	return []string{"list", "--json-lines", "--lock-wait", lockWait, "--", archiveRef(repo, archive)}
}

// flexString accepts a JSON string or number; borg reports a numeric user or
// group when the id has no name on the machine that runs borg.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

func (v1) ParseItem(line []byte) (Item, error) {
	var raw struct {
		Path       string     `json:"path"`
		Type       string     `json:"type"`
		Mode       string     `json:"mode"`
		User       flexString `json:"user"`
		Group      flexString `json:"group"`
		Size       int64      `json:"size"`
		Mtime      string     `json:"mtime"`
		LinkTarget string     `json:"linktarget"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return Item{}, err
	}
	return Item{Path: raw.Path, Name: path.Base(raw.Path), Type: raw.Type, Mode: raw.Mode, User: string(raw.User),
		Group: string(raw.Group), Size: raw.Size, Mtime: raw.Mtime, LinkTarget: raw.LinkTarget}, nil
}

// Paths are passed with the "pp:" (path prefix) style, so that they are never
// interpreted as regular expressions or shell patterns.
func (v1) ExtractStdoutArgs(repo, archive, p string) []string {
	return []string{"extract", "--stdout", "--lock-wait", lockWait, "--", archiveRef(repo, archive), "pp:" + p}
}

func (v1) ExportTarArgs(repo, archive string, paths []string) []string {
	args := []string{"export-tar", "--lock-wait", lockWait, "--", archiveRef(repo, archive), "-"}
	for _, p := range paths {
		args = append(args, "pp:"+p)
	}
	return args
}

func (v1) CreateArgs(repo, archive string, opts CreateOptions) []string {
	args := []string{"create", "--info", "--stats", "--show-rc", "--lock-wait", lockWait}
	if opts.Compression != "" {
		args = append(args, "--compression", opts.Compression)
	}
	if opts.OneFileSystem {
		args = append(args, "--one-file-system")
	}
	for _, e := range opts.Excludes {
		args = append(args, "--exclude="+e)
	}
	args = append(args, "--", archiveRef(repo, archive))
	return append(args, opts.Paths...)
}

func (v1) PruneArgs(repo, glob string, keep Retention) []string {
	args := []string{"prune", "--info", "--list", "--stats", "--show-rc", "--lock-wait", lockWait}
	add := func(flag string, n int) {
		if n > 0 {
			args = append(args, flag, strconv.Itoa(n))
		}
	}
	add("--keep-hourly", keep.Hourly)
	add("--keep-daily", keep.Daily)
	add("--keep-weekly", keep.Weekly)
	add("--keep-monthly", keep.Monthly)
	add("--keep-yearly", keep.Yearly)
	if glob != "" {
		args = append(args, "--glob-archives", glob)
	}
	return append(args, "--", repo)
}

func (v1) DeleteArgs(repo, archive string) []string {
	return []string{"delete", "--info", "--stats", "--show-rc", "--lock-wait", lockWait, "--", archiveRef(repo, archive)}
}

func (v1) CompactArgs(repo string) []string {
	return []string{"compact", "--info", "--lock-wait", lockWait, "--", repo}
}
