package bluos

import (
	"encoding/xml"
	"strings"
	"unicode"
)

func detectPlaybackSource(status Status) string {
	attrs := statusAttrMap(status.AnyAttrs)
	elems := statusElementMap(status.AnyElems)
	keys := []string{"service", "source", "input", "inputtype", "stream", "transport", "mode", "sid", "pid", "context"}

	for _, key := range keys {
		if v, ok := attrs[key]; ok {
			if source := classifySource(v, true); source != "" {
				return source
			}
		}
		if v, ok := elems[key]; ok {
			if source := classifySource(v, true); source != "" {
				return source
			}
		}
	}

	for _, attr := range status.AnyAttrs {
		if source := classifySource(attr.Value, false); source != "" {
			return source
		}
	}
	for _, elem := range status.AnyElems {
		if source := classifySource(elem.Value, false); source != "" {
			return source
		}
		for _, attr := range elem.Attrs {
			if source := classifySource(attr.Value, false); source != "" {
				return source
			}
		}
	}

	return ""
}

func statusAttrMap(attrs []xml.Attr) map[string]string {
	out := map[string]string{}
	for _, attr := range attrs {
		k := strings.ToLower(strings.TrimSpace(attr.Name.Local))
		v := strings.TrimSpace(attr.Value)
		if k == "" || v == "" {
			continue
		}
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}

func statusElementMap(elems []statusAnyElement) map[string]string {
	out := map[string]string{}
	for _, elem := range elems {
		k := strings.ToLower(strings.TrimSpace(elem.XMLName.Local))
		v := strings.TrimSpace(elem.Value)
		if k == "" || v == "" {
			continue
		}
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}

func classifySource(raw string, allowFallback bool) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	v := strings.ToLower(raw)

	switch {
	case strings.Contains(v, "spotify"):
		return "Spotify"
	case strings.Contains(v, "airplay"), strings.Contains(v, "raop"):
		return "AirPlay"
	case strings.Contains(v, "bluetooth"), strings.Contains(v, "a2dp"), strings.Contains(v, " bt") || strings.HasPrefix(v, "bt"):
		return "Bluetooth"
	case strings.Contains(v, "tunein"):
		return "TuneIn"
	case strings.Contains(v, "linein"), strings.Contains(v, "line in"), strings.Contains(v, "aux"):
		return "Line In"
	case strings.Contains(v, "optical"), strings.Contains(v, "spdif"), strings.Contains(v, "toslink"):
		return "Optical In"
	case strings.Contains(v, "hdmi"), strings.Contains(v, "earc"), strings.Contains(v, " arc"):
		return "HDMI ARC"
	case strings.Contains(v, "usb"):
		return "USB"
	case strings.Contains(v, "capture"):
		return "Input"
	}
	if !allowFallback {
		return ""
	}

	// Friendly fallback for unknown service values.
	fallback := strings.TrimSpace(raw)
	if fallback == "" {
		return ""
	}
	if !containsLetter(fallback) {
		return ""
	}
	if strings.EqualFold(fallback, "radio") {
		return "Radio"
	}
	if strings.EqualFold(fallback, "library") {
		return "Library"
	}
	if len(fallback) > 64 {
		fallback = fallback[:64]
	}
	return fallback
}

func containsLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
