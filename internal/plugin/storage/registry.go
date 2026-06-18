// registry.go — Factory + scheme registry for StoragePlugin implementations (file://, s3://, …).
package storage

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"sync"
)

// Factory builds a fresh StoragePlugin instance. Implementations register
// one Factory per URL scheme (fs registers "file", s3 registers "s3", ...).
type Factory func() StoragePlugin

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register installs a Factory for a URL scheme. Panics on duplicate
// registration (programmer error, not a runtime condition). Call from
// `func init()` of the plugin package.
func Register(scheme string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[scheme]; ok {
		panic("storage: scheme " + scheme + " already registered")
	}
	registry[scheme] = f
}

// Schemes returns the registered scheme names in sorted order.
func Schemes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Open parses rawURL, looks up the matching Factory, builds a plugin,
// and Opens it against the parsed config. The returned plugin is the
// caller's responsibility to Close.
//
// On macOS / Linux a bare absolute path is also accepted as a convenience
// and treated as file://<path>. (URLs without a scheme are rejected
// otherwise, to avoid silently turning typos into fs operations.)
func Open(ctx context.Context, rawURL string) (StoragePlugin, error) {
	u, err := parseStorageURL(rawURL)
	if err != nil {
		return nil, err
	}
	registryMu.RLock()
	f, ok := registry[u.Scheme]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q (registered: %v)", ErrUnknownScheme, u.Scheme, Schemes())
	}
	plugin := f()
	if err := plugin.Open(ctx, StorageConfig{URL: u}); err != nil {
		_ = plugin.Close()
		return nil, err
	}
	return plugin, nil
}

// parseStorageURL canonicalizes the user-supplied URL. We're tolerant of
// the common shapes — bare absolute paths and `file:/path` (single slash) —
// but firm about anything ambiguous.
func parseStorageURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("storage: empty URL")
	}
	if len(raw) > 0 && raw[0] == '/' {
		// Bare absolute path -> file://<path>.
		return &url.URL{Scheme: "file", Path: raw}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("storage: parse %q: %w", raw, err)
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("storage: URL %q has no scheme; use file://, s3://, ...", raw)
	}
	return u, nil
}
