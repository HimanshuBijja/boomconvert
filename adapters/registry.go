package adapters

import (
	"context"
	"strings"
	"sync"
)

type ConvertOptions struct {
	Quality int
}

type Adapter interface {
	Name() string
	RequiredTool() string
	Supports(srcFormat, dstFormat string) bool
	Convert(ctx context.Context, src, dst string, opts ConvertOptions) error
}

type Rule struct {
	Source  string `json:"source"`
	Target  string `json:"target"`
	Adapter string `json:"adapter"`
	Tool    string `json:"tool"`
}

type ToolChecker interface {
	Available(id string) bool
}

type Registry struct {
	mu       sync.RWMutex
	adapters []Adapter
	checker  ToolChecker
}

func NewRegistry(checker ToolChecker) *Registry {
	return &Registry{checker: checker}
}

func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters = append(r.adapters, a)
}

func (r *Registry) Lookup(srcFormat, dstFormat string) (Adapter, bool) {
	all := r.LookupAll(srcFormat, dstFormat)
	if len(all) == 0 {
		return nil, false
	}
	return all[0], true
}

// LookupAll returns every adapter that supports the requested conversion,
// in registration order. Used by the converter to fall back to the next
// adapter when the preferred one fails (e.g. UnoAdapter -> LibreOfficeAdapter).
func (r *Registry) LookupAll(srcFormat, dstFormat string) []Adapter {
	s := Canonical(srcFormat)
	d := Canonical(dstFormat)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Adapter
	for _, a := range r.adapters {
		if !r.checker.Available(a.RequiredTool()) {
			continue
		}
		if a.Supports(s, d) {
			out = append(out, a)
		}
	}
	return out
}

func (r *Registry) AvailableRules() []Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Rule
	for _, a := range r.adapters {
		if !r.checker.Available(a.RequiredTool()) {
			continue
		}
		if lister, ok := a.(rulesLister); ok {
			for _, rule := range lister.Rules() {
				rule.Adapter = a.Name()
				rule.Tool = a.RequiredTool()
				out = append(out, rule)
			}
		}
	}
	return out
}

type rulesLister interface {
	Rules() []Rule
}

func Canonical(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	switch ext {
	case "jpeg":
		return "jpg"
	case "tif":
		return "tiff"
	}
	return ext
}
