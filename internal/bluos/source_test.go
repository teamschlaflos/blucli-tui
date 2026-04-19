package bluos

import (
	"encoding/xml"
	"testing"
)

func TestDetectPlaybackSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status Status
		want   string
	}{
		{
			name:   "spotify attr",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "service"}, Value: "Spotify"}}},
			want:   "Spotify",
		},
		{
			name:   "airplay attr",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "source"}, Value: "raop"}}},
			want:   "AirPlay",
		},
		{
			name:   "bluetooth attr",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "inputType"}, Value: "A2DP"}}},
			want:   "Bluetooth",
		},
		{
			name:   "line in attr",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "input"}, Value: "linein"}}},
			want:   "Line In",
		},
		{
			name:   "unknown service fallback",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "service"}, Value: "Tidal"}}},
			want:   "Tidal",
		},
		{
			name:   "numeric value ignored",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "sid"}, Value: "1"}}},
			want:   "",
		},
		{
			name: "service element",
			status: Status{
				AnyElems: []statusAnyElement{{XMLName: xml.Name{Local: "service"}, Value: "Spotify"}},
			},
			want: "Spotify",
		},
		{
			name:   "ignore unrelated attrs",
			status: Status{AnyAttrs: []xml.Attr{{Name: xml.Name{Local: "foo"}, Value: "bar"}}},
			want:   "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := detectPlaybackSource(tt.status); got != tt.want {
				t.Fatalf("detectPlaybackSource() = %q; want %q", got, tt.want)
			}
		})
	}
}
