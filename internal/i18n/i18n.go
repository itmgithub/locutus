// Package i18n provides message translation. Each language is a flat JSON
// file in the locales directory (e.g. locales/nl.json); adding a language
// only requires adding a new file with the same keys. The key "lang.name" is
// the name of the language in that language, shown in the language menu.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
)

// Cookie holds the language chosen by the user.
const Cookie = "lang"

// Language is one available language.
type Language struct {
	Code string
	Name string
}

//go:embed locales/*.json
var localeFS embed.FS

// Translator holds the message catalogs of all available languages.
type Translator struct {
	defaultLang string
	catalogs    map[string]map[string]string
}

// New loads all embedded catalogs. defaultLang must be one of them.
func New(defaultLang string) (*Translator, error) {
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		return nil, err
	}
	t := &Translator{defaultLang: defaultLang, catalogs: map[string]map[string]string{}}
	for _, e := range entries {
		data, err := localeFS.ReadFile(path.Join("locales", e.Name()))
		if err != nil {
			return nil, err
		}
		catalog := map[string]string{}
		if err := json.Unmarshal(data, &catalog); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		t.catalogs[strings.TrimSuffix(e.Name(), ".json")] = catalog
	}
	if _, ok := t.catalogs[defaultLang]; !ok {
		return nil, fmt.Errorf("default language %q has no catalog", defaultLang)
	}
	return t, nil
}

// T translates key into lang. Extra args are applied with fmt.Sprintf.
// Unknown keys fall back to the default language and finally to the key itself.
func (t *Translator) T(lang, key string, args ...any) string {
	msg, ok := t.catalogs[lang][key]
	if !ok {
		msg, ok = t.catalogs[t.defaultLang][key]
	}
	if !ok {
		msg = key
	}
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// Has reports whether lang is available.
func (t *Translator) Has(lang string) bool {
	_, ok := t.catalogs[lang]
	return ok
}

// Languages returns the available languages, sorted by name.
func (t *Translator) Languages() []Language {
	list := make([]Language, 0, len(t.catalogs))
	for code, c := range t.catalogs {
		name := c["lang.name"]
		if name == "" {
			name = code
		}
		list = append(list, Language{Code: code, Name: name})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Language picks the language for a request: the "lang" cookie first, then
// the Accept-Language header, then the default language.
func (t *Translator) Language(r *http.Request) string {
	if c, err := r.Cookie(Cookie); err == nil {
		if _, ok := t.catalogs[c.Value]; ok {
			return c.Value
		}
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		tag = strings.ToLower(strings.SplitN(tag, "-", 2)[0])
		if _, ok := t.catalogs[tag]; ok {
			return tag
		}
	}
	return t.defaultLang
}
