package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
)

type DiscoveryCache struct {
	UpdatedAt time.Time         `json:"updated_at"`
	Devices   []Device          `json:"devices"`
	Rows      []CachedPlayerRow `json:"rows,omitempty"`
}

type CachedPlayerRow struct {
	Device        Device `json:"device"`
	Name          string `json:"name,omitempty"`
	Volume        int    `json:"volume,omitempty"`
	Mute          bool   `json:"mute,omitempty"`
	PlaybackState string `json:"playback_state,omitempty"`
	Indent        int    `json:"indent,omitempty"`
	IsGroup       bool   `json:"is_group,omitempty"`
	TellSlaves    bool   `json:"tell_slaves,omitempty"`
	NowPlaying    string `json:"now_playing,omitempty"`
	Source        string `json:"source,omitempty"`
	Grouping      string `json:"grouping,omitempty"`
	GroupDetail   string `json:"group_detail,omitempty"`
	StatusDetail  string `json:"status_detail,omitempty"`
	Kind          string `json:"kind,omitempty"`
}

func NewDiscoveryCache(updatedAt time.Time, devices []Device) DiscoveryCache {
	return NewDiscoveryCacheWithRows(updatedAt, devices, nil)
}

func NewDiscoveryCacheWithRows(updatedAt time.Time, devices []Device, rows []CachedPlayerRow) DiscoveryCache {
	out := make([]Device, 0, len(devices))
	for _, device := range devices {
		if device.Host == "" || device.Port == 0 {
			continue
		}
		if device.ID == "" {
			device.ID = fmt.Sprintf("%s:%d", device.Host, device.Port)
		}
		out = append(out, device)
	}
	outRows := make([]CachedPlayerRow, 0, len(rows))
	for _, row := range rows {
		if row.Device.Host == "" || row.Device.Port == 0 {
			continue
		}
		if row.Device.ID == "" {
			row.Device.ID = fmt.Sprintf("%s:%d", row.Device.Host, row.Device.Port)
		}
		outRows = append(outRows, row)
	}
	return DiscoveryCache{UpdatedAt: updatedAt, Devices: out, Rows: outRows}
}

func LoadDiscoveryCache(path string) (DiscoveryCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DiscoveryCache{}, err
	}

	var cache DiscoveryCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return DiscoveryCache{}, err
	}
	return cache, nil
}

func SaveDiscoveryCache(path string, cache DiscoveryCache) error {
	if err := ensureParentDir(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func (c DiscoveryCache) Lookup(idOrHostPort string) (Device, bool) {
	for _, device := range c.Devices {
		if device.ID == idOrHostPort {
			return device, true
		}
		if fmt.Sprintf("%s:%d", device.Host, device.Port) == idOrHostPort {
			return device, true
		}
	}
	return Device{}, false
}

func (c DiscoveryCache) FindByName(query string) []Device {
	q := normalizeName(query)
	if q == "" {
		return nil
	}

	var exact []Device
	var fuzzy []Device
	for _, device := range c.Devices {
		n := normalizeName(device.Name)
		if n == "" {
			continue
		}
		if n == q {
			exact = append(exact, device)
			continue
		}
		if strings.Contains(n, q) || strings.Contains(q, n) {
			fuzzy = append(fuzzy, device)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fuzzy
}

func normalizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
