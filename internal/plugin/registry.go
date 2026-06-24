package plugin

import "sync"

var (
	mu      sync.RWMutex
	plugins = map[string]MetricsPlugin{}
)

// Register adds p to the global registry under p.Type().
// Conventionally called from init() in each plugin package.
func Register(p MetricsPlugin) {
	mu.Lock()
	defer mu.Unlock()
	plugins[p.Type()] = p
}

// Get retrieves the plugin registered for typeName.
func Get(typeName string) (MetricsPlugin, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := plugins[typeName]
	return p, ok
}
