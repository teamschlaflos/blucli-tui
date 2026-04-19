package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/steipete/blucli/internal/bluos"
	"github.com/steipete/blucli/internal/config"
)

func TestCollectTUIDevicesFromCacheNoDiscover(t *testing.T) {
	t.Parallel()

	cache := config.DiscoveryCache{Devices: []config.Device{
		{ID: "b:11000", Host: "b", Port: 11000, Name: "Bravo"},
		{ID: "a:11000", Host: "a", Port: 11000, Name: "Alpha"},
	}}

	devices, err := collectTUIDevices(context.Background(), config.Config{}, cache, "", false, time.Second)
	if err != nil {
		t.Fatalf("collectTUIDevices error: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("len(devices) = %d; want 2", len(devices))
	}
	if got := displayDeviceName(devices[0]); got != "Alpha" {
		t.Fatalf("first device = %q; want Alpha", got)
	}
	if got := displayDeviceName(devices[1]); got != "Bravo" {
		t.Fatalf("second device = %q; want Bravo", got)
	}
}

func TestCollectTUIDevicesExplicitDevice(t *testing.T) {
	t.Parallel()

	devices, err := collectTUIDevices(context.Background(), config.Config{}, config.DiscoveryCache{}, "192.0.2.44:11001", false, time.Second)
	if err != nil {
		t.Fatalf("collectTUIDevices error: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("len(devices) = %d; want 1", len(devices))
	}
	if devices[0].Host != "192.0.2.44" || devices[0].Port != 11001 {
		t.Fatalf("device = %+v; want 192.0.2.44:11001", devices[0])
	}
}

func TestNewPlayersModelInitialStatusLine(t *testing.T) {
	t.Parallel()

	m := newPlayersModel(
		context.Background(),
		config.Config{},
		config.DiscoveryCache{},
		"",
		"Invalid cache state (ignoring)",
		"",
		true,
		time.Second,
		time.Second,
		false,
		nil,
	)
	if m.statusLine != "Invalid cache state (ignoring)" {
		t.Fatalf("statusLine=%q", m.statusLine)
	}
	if m.statusAt.IsZero() {
		t.Fatalf("statusAt is zero; want timestamp")
	}
}

func TestSummarizeGrouping(t *testing.T) {
	t.Parallel()

	name, detail := summarizeGrouping(bluos.SyncStatus{}, nil)
	if name != "Ungrouped" || detail != "No linked players" {
		t.Fatalf("ungrouped = (%q, %q)", name, detail)
	}

	name, detail = summarizeGrouping(bluos.SyncStatus{Group: "Living", Slaves: []bluos.SyncSlave{{ID: "kitchen"}, {ID: "office"}, {ID: "bedroom"}}}, nil)
	if name != "Master (Living)" {
		t.Fatalf("name = %q; want Master (Living)", name)
	}
	if !strings.Contains(detail, "Slaves:") {
		t.Fatalf("detail = %q; want slave list", detail)
	}

	name, detail = summarizeGrouping(bluos.SyncStatus{Master: &bluos.SyncMaster{Host: "192.0.2.10", Port: 11000}}, nil)
	if name != "Slave" || !strings.Contains(detail, "192.0.2.10:11000") {
		t.Fatalf("slave = (%q, %q)", name, detail)
	}

	name, detail = summarizeGrouping(bluos.SyncStatus{}, context.DeadlineExceeded)
	if name != "Grouping unavailable" || !strings.Contains(detail, "deadline") {
		t.Fatalf("err = (%q, %q)", name, detail)
	}
}

func TestRunTUIRequiresTerminal(t *testing.T) {
	t.Parallel()

	cfgPath := writeTestConfig(t, "http://127.0.0.1:11000")

	var out bytes.Buffer
	var errOut bytes.Buffer
	code := Run(
		context.Background(),
		[]string{"--config", cfgPath, "--discover=false", "--device", "192.0.2.1:11000", "tui"},
		&out,
		&errOut,
	)
	if code != 2 {
		t.Fatalf("exit code = %d; stderr=%q", code, errOut.String())
	}
	if got := errOut.String(); !strings.Contains(got, "interactive terminal") {
		t.Fatalf("stderr = %q; want interactive terminal error", got)
	}
}

func TestBuildRowsFromSnapshotsAddsSlaveByHostFallback(t *testing.T) {
	t.Parallel()

	master := playerSnapshot{
		Device: config.Device{ID: "192.0.2.10:11000", Host: "192.0.2.10", Port: 11000, Name: "Player One"},
		Key:    "192.0.2.10:11000",
		Name:   "Player One",
		Volume: 29,
		Sync: bluos.SyncStatus{
			Group:  "Player One+Player Ü",
			Slaves: []bluos.SyncSlave{{ID: "192.0.2.11"}},
		},
	}
	slave := playerSnapshot{
		Device: config.Device{ID: "192.0.2.11:11000", Host: "192.0.2.11", Port: 11000, Name: "Player Ü"},
		Key:    "192.0.2.11:11000",
		Name:   "Player Ü",
		Volume: 25,
		Sync: bluos.SyncStatus{
			Group:  "Player One+Player Ü",
			Master: &bluos.SyncMaster{Host: "192.0.2.10", Port: 11000},
		},
	}

	rows := buildRowsFromSnapshots([]playerSnapshot{master, slave})
	if len(rows) < 3 {
		t.Fatalf("rows=%d; want at least 3", len(rows))
	}

	foundGroup := false
	foundPlayerOne := false
	foundPlayerU := false
	for _, row := range rows {
		if row.IsGroup && strings.Contains(row.Name, "Player One+Player Ü") {
			foundGroup = true
		}
		if !row.IsGroup && row.Indent == 3 && row.Name == "Player One" {
			foundPlayerOne = true
		}
		if !row.IsGroup && row.Indent == 3 && row.Name == "Player Ü" {
			foundPlayerU = true
		}
	}

	if !foundGroup || !foundPlayerOne || !foundPlayerU {
		t.Fatalf("missing expected rows: group=%t player_one=%t player_u=%t rows=%+v", foundGroup, foundPlayerOne, foundPlayerU, rows)
	}
}

func TestNowPlayingTextRequiresPlayingState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status bluos.Status
		want   string
	}{
		{
			name:   "paused hides metadata",
			status: bluos.Status{State: "pause", Artist: "A", Title: "T"},
			want:   "",
		},
		{
			name:   "stopped hides metadata",
			status: bluos.Status{State: "stop", Artist: "A", Title: "T"},
			want:   "",
		},
		{
			name:   "playing shows metadata",
			status: bluos.Status{State: "play", Artist: "A", Title: "T"},
			want:   "A - T",
		},
		{
			name:   "streaming no metadata",
			status: bluos.Status{State: "stream"},
			want:   "PLAYING",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nowPlayingText(tt.status); got != tt.want {
				t.Fatalf("nowPlayingText() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestPlaybackToggleAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
		want  string
	}{
		{name: "play pauses", state: "play", want: "pause"},
		{name: "playing pauses", state: "playing", want: "pause"},
		{name: "stream pauses", state: "stream", want: "pause"},
		{name: "pause plays", state: "pause", want: "play"},
		{name: "stop plays", state: "stop", want: "play"},
		{name: "empty plays", state: "", want: "play"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := playbackToggleAction(tt.state); got != tt.want {
				t.Fatalf("playbackToggleAction(%q) = %q; want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestFormatHeadlineLineAlignsUnicodeNames(t *testing.T) {
	t.Parallel()

	width := 64
	line1 := formatHeadlineLine("  ", "Player Ü", "45%", width)
	line2 := formatHeadlineLine("> ", "Player One", "44%", width)

	if got := textWidth(line1); got != width {
		t.Fatalf("line1 width=%d; want %d (%q)", got, width, line1)
	}
	if got := textWidth(line2); got != width {
		t.Fatalf("line2 width=%d; want %d (%q)", got, width, line2)
	}

	left1 := strings.TrimSuffix(line1, "45%")
	left2 := strings.TrimSuffix(line2, "44%")
	if textWidth(left1) != textWidth(left2) {
		t.Fatalf("right column misaligned: %s vs %s", fmt.Sprintf("%q", line1), fmt.Sprintf("%q", line2))
	}
}

func TestSanitizeTerminalTextRemovesControlChars(t *testing.T) {
	t.Parallel()

	got := sanitizeTerminalText("  Player\x1b[31m One\r\n\t")
	if got == "" {
		t.Fatalf("sanitizeTerminalText() empty; want non-empty")
	}
	if containsControlRunes(got) {
		t.Fatalf("sanitizeTerminalText()=%q contains control runes", got)
	}
}

func TestRenderPlayersBodyLinesSanitizesUntrustedText(t *testing.T) {
	t.Parallel()

	rows := []playerRow{
		{
			Name:       "Player\x1b]8;;http://example.test\x07 One",
			NowPlaying: "Track\r\nLine",
			Source:     "Spotify\x1b[0m",
			Volume:     42,
			Kind:       rowKindStandalone,
		},
	}

	lines := renderPlayersBodyLines(rows, 0, 80, 12, lipgloss.NewStyle(), false)
	if len(lines) == 0 {
		t.Fatalf("renderPlayersBodyLines() returned no lines")
	}
	for _, line := range lines {
		if containsControlRunes(line) {
			t.Fatalf("rendered line contains control runes: %q", line)
		}
	}
}

func TestSpacingLinesBetweenRows(t *testing.T) {
	t.Parallel()

	normal := playerRow{Name: "Player Zero", Kind: rowKindStandalone}
	group := playerRow{Name: "Player One+Player Ü", IsGroup: true, Kind: rowKindGroup}
	memberA := playerRow{Name: "Player Ü", Indent: 3, Kind: rowKindGroupMember}
	memberB := playerRow{Name: "Player One", Indent: 3, Kind: rowKindGroupMember}

	if got := len(spacingLinesBetweenRows(normal, group)); got != 2 {
		t.Fatalf("normal->group spacing=%d; want 2", got)
	}
	if got := len(spacingLinesBetweenRows(group, normal)); got != 2 {
		t.Fatalf("group->normal spacing=%d; want 2", got)
	}
	if got := len(spacingLinesBetweenRows(group, memberA)); got != 0 {
		t.Fatalf("group->member spacing=%d; want 0", got)
	}
	if got := len(spacingLinesBetweenRows(memberA, memberB)); got != 0 {
		t.Fatalf("member->member spacing=%d; want 0", got)
	}
	if got := len(spacingLinesBetweenRows(memberB, normal)); got != 2 {
		t.Fatalf("member->normal spacing=%d; want 2", got)
	}
}

func TestFindMasterRowForMember(t *testing.T) {
	t.Parallel()

	rows := []playerRow{
		{Name: "GroupA", IsGroup: true, Kind: rowKindGroup, Device: config.Device{ID: "a:11000", Host: "a", Port: 11000}},
		{Name: "Member1", Indent: 3, Kind: rowKindGroupMember, Device: config.Device{ID: "b:11000", Host: "b", Port: 11000}},
		{Name: "Member2", Indent: 3, Kind: rowKindGroupMember, Device: config.Device{ID: "c:11000", Host: "c", Port: 11000}},
		{Name: "Solo", Kind: rowKindStandalone, Device: config.Device{ID: "d:11000", Host: "d", Port: 11000}},
	}

	master, ok := findMasterRowForMember(rows, 2)
	if !ok {
		t.Fatalf("findMasterRowForMember() ok=false; want true")
	}
	if master.Name != "GroupA" {
		t.Fatalf("master.Name=%q; want GroupA", master.Name)
	}

	if _, ok := findMasterRowForMember(rows, 3); ok {
		t.Fatalf("findMasterRowForMember() for standalone row ok=true; want false")
	}
}

func TestGroupKeyCanRememberGroupAndApplyToPlayer(t *testing.T) {
	t.Parallel()

	m := playersModel{
		rows: []playerRow{
			{Name: "Player One+Player Ü", IsGroup: true, Kind: rowKindGroup, Device: config.Device{ID: "a:11000", Host: "a", Port: 11000}},
			{Name: "Player Two", Kind: rowKindStandalone, Device: config.Device{ID: "b:11000", Host: "b", Port: 11000}},
		},
		selected: 0,
	}

	keyG := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}

	model1, cmd1 := m.Update(keyG)
	if cmd1 != nil {
		t.Fatalf("first g on group returned cmd; want nil")
	}
	m1, ok := model1.(playersModel)
	if !ok {
		t.Fatalf("model1 type=%T; want playersModel", model1)
	}
	if got := m1.groupPendingKey; got != "a:11000" {
		t.Fatalf("groupPendingKey=%q; want a:11000", got)
	}
	if !strings.Contains(m1.statusLine, "Selected group") {
		t.Fatalf("statusLine=%q; want selected group hint", m1.statusLine)
	}

	m1.selected = 1
	model2, cmd2 := m1.Update(keyG)
	if cmd2 == nil {
		t.Fatalf("second g (group->player) returned nil cmd; want grouping command")
	}
	m2, ok := model2.(playersModel)
	if !ok {
		t.Fatalf("model2 type=%T; want playersModel", model2)
	}
	if m2.groupPendingKey != "" {
		t.Fatalf("groupPendingKey=%q; want cleared", m2.groupPendingKey)
	}
	if !m2.loading {
		t.Fatalf("loading=%t; want true", m2.loading)
	}
}

func TestInputModalOpenAndClose(t *testing.T) {
	t.Parallel()

	m := playersModel{
		rows: []playerRow{
			{Name: "Player Zero", Kind: rowKindStandalone, Device: config.Device{ID: "a:11000", Host: "a", Port: 11000}},
		},
		selected: 0,
	}

	openKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{':'}}
	model1, cmd1 := m.Update(openKey)
	if cmd1 == nil {
		t.Fatalf("open ':' returned nil cmd; want options-loading cmd")
	}
	m1, ok := model1.(playersModel)
	if !ok {
		t.Fatalf("model1 type=%T; want playersModel", model1)
	}
	if !m1.inputModalOpen {
		t.Fatalf("inputModalOpen=%t; want true", m1.inputModalOpen)
	}
	if !m1.inputModalLoading {
		t.Fatalf("inputModalLoading=%t; want true", m1.inputModalLoading)
	}
	if len(m1.inputModalOptions) != 1 {
		t.Fatalf("len(inputModalOptions)=%d; want 1", len(m1.inputModalOptions))
	}

	escKey := tea.KeyMsg{Type: tea.KeyEsc}
	model2, cmd2 := m1.Update(escKey)
	if cmd2 != nil {
		t.Fatalf("esc returned cmd; want nil")
	}
	m2, ok := model2.(playersModel)
	if !ok {
		t.Fatalf("model2 type=%T; want playersModel", model2)
	}
	if m2.inputModalOpen {
		t.Fatalf("inputModalOpen=%t; want false", m2.inputModalOpen)
	}
	if m2.inputModalLoading {
		t.Fatalf("inputModalLoading=%t; want false", m2.inputModalLoading)
	}
}

func TestInputModalConfirmReturnsCommand(t *testing.T) {
	t.Parallel()

	m := playersModel{
		rows: []playerRow{
			{Name: "Player Zero", Kind: rowKindStandalone, Device: config.Device{ID: "a:11000", Host: "a", Port: 11000}},
		},
		selected:       0,
		httpTimeout:    time.Second,
		inputModalOpen: true,
	}

	enterKey := tea.KeyMsg{Type: tea.KeyEnter}
	model, cmd := m.Update(enterKey)
	if cmd == nil {
		t.Fatalf("enter returned nil cmd; want input-change command")
	}
	m1, ok := model.(playersModel)
	if !ok {
		t.Fatalf("model type=%T; want playersModel", model)
	}
	if m1.inputModalOpen {
		t.Fatalf("inputModalOpen=%t; want false", m1.inputModalOpen)
	}
	if !m1.loading {
		t.Fatalf("loading=%t; want true", m1.loading)
	}
}

func TestLoadInputModalOptionsCmdAddsHDMIARC(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/RadioBrowse" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("service"); got != "Capture" {
			t.Fatalf("service=%q; want Capture", got)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<radiotime service="Capture"><item id="HDMI" text="HDMI ARC" inputType="capture" URL="Capture%3Ahdmi%3Aarc"/></radiotime>`))
	}))
	t.Cleanup(srv.Close)

	device, err := config.ParseDevice(srv.URL)
	if err != nil {
		t.Fatalf("ParseDevice() err = %v", err)
	}

	m := playersModel{
		ctx:         context.Background(),
		httpTimeout: time.Second,
	}
	row := playerRow{Name: "Player One", Device: device}
	base := defaultInputModalOptions()
	cmd := m.loadInputModalOptionsCmd(row, deviceKey(row.Device), base)
	if cmd == nil {
		t.Fatalf("loadInputModalOptionsCmd() returned nil command")
	}
	rawMsg := cmd()
	msg, ok := rawMsg.(playersInputOptionsLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T; want playersInputOptionsLoadedMsg", rawMsg)
	}
	if msg.Err != nil {
		t.Fatalf("msg.Err = %v; want nil", msg.Err)
	}
	if len(msg.Options) != 2 {
		t.Fatalf("len(msg.Options) = %d; want 2", len(msg.Options))
	}
	if msg.Options[1].Input != "HDMI ARC" {
		t.Fatalf("msg.Options[1].Input = %q; want HDMI ARC", msg.Options[1].Input)
	}
	if msg.Options[1].URL != "Capture:hdmi:arc" {
		t.Fatalf("msg.Options[1].URL = %q; want Capture:hdmi:arc", msg.Options[1].URL)
	}
}

func TestLoadInputModalOptionsCmdSkipsHDMIARCWhenUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/RadioBrowse" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<radiotime service="Capture"><item id="LineIn" text="Line In" URL="Capture%3Alinein"/></radiotime>`))
	}))
	t.Cleanup(srv.Close)

	device, err := config.ParseDevice(srv.URL)
	if err != nil {
		t.Fatalf("ParseDevice() err = %v", err)
	}

	m := playersModel{
		ctx:         context.Background(),
		httpTimeout: time.Second,
	}
	row := playerRow{Name: "Player One", Device: device}
	base := defaultInputModalOptions()
	cmd := m.loadInputModalOptionsCmd(row, deviceKey(row.Device), base)
	if cmd == nil {
		t.Fatalf("loadInputModalOptionsCmd() returned nil command")
	}
	rawMsg := cmd()
	msg, ok := rawMsg.(playersInputOptionsLoadedMsg)
	if !ok {
		t.Fatalf("msg type = %T; want playersInputOptionsLoadedMsg", rawMsg)
	}
	if msg.Err != nil {
		t.Fatalf("msg.Err = %v; want nil", msg.Err)
	}
	if len(msg.Options) != 1 {
		t.Fatalf("len(msg.Options) = %d; want 1", len(msg.Options))
	}
	if msg.Options[0].Input != "Spotify Connect" {
		t.Fatalf("msg.Options[0].Input = %q; want Spotify Connect", msg.Options[0].Input)
	}
}

func TestChangeInputCmdUsesSelectedOptionURL(t *testing.T) {
	t.Parallel()

	var playURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Play" {
			http.NotFound(w, r)
			return
		}
		playURL = r.URL.Query().Get("url")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<ok/>`))
	}))
	t.Cleanup(srv.Close)

	device, err := config.ParseDevice(srv.URL)
	if err != nil {
		t.Fatalf("ParseDevice() err = %v", err)
	}

	m := playersModel{
		ctx:         context.Background(),
		httpTimeout: time.Second,
	}
	row := playerRow{Name: "Player One", Device: device}
	option := playersInputOption{Label: "HDMI ARC", Input: "HDMI ARC", URL: "Capture:hdmi:arc"}
	cmd := m.changeInputCmd(row, option)
	if cmd == nil {
		t.Fatalf("changeInputCmd() returned nil command")
	}
	rawMsg := cmd()
	msg, ok := rawMsg.(playersInputChangedMsg)
	if !ok {
		t.Fatalf("msg type = %T; want playersInputChangedMsg", rawMsg)
	}
	if msg.Err != nil {
		t.Fatalf("msg.Err = %v; want nil", msg.Err)
	}
	if msg.Input != "HDMI ARC" {
		t.Fatalf("msg.Input = %q; want HDMI ARC", msg.Input)
	}
	if playURL != "Capture:hdmi:arc" {
		t.Fatalf("play url = %q; want Capture:hdmi:arc", playURL)
	}
}

func TestFormatCompactMemberLineAlignsVolumeColumn(t *testing.T) {
	t.Parallel()

	width := 64
	nameCol := 24
	line1 := formatCompactMemberLine("  ", "   Player Ü", "45%", width, nameCol)
	line2 := formatCompactMemberLine("> ", "   Player One", "44%", width, nameCol)

	if got := textWidth(line1); got > width {
		t.Fatalf("line1 width=%d; want <=%d (%q)", got, width, line1)
	}
	if got := textWidth(line2); got > width {
		t.Fatalf("line2 width=%d; want <=%d (%q)", got, width, line2)
	}

	left1 := strings.TrimSuffix(line1, "45%")
	left2 := strings.TrimSuffix(line2, "44%")
	if textWidth(left1) != textWidth(left2) {
		t.Fatalf("member volume column misaligned: %q vs %q", line1, line2)
	}
}

func TestRenderPlayersBodyLinesLoadingState(t *testing.T) {
	t.Parallel()

	lines := renderPlayersBodyLines(nil, 0, 80, 20, lipgloss.NewStyle(), true)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Loading players...") {
		t.Fatalf("loading lines=%q; want loading message", joined)
	}
}

func TestRenderPlayersBodyLinesNoPlayersState(t *testing.T) {
	t.Parallel()

	lines := renderPlayersBodyLines(nil, 0, 80, 20, lipgloss.NewStyle(), false)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "No players found.") {
		t.Fatalf("no-player lines=%q; want no-players message", joined)
	}
}

func TestCacheRowsRoundTripPreservesGroupingState(t *testing.T) {
	t.Parallel()

	rows := []playerRow{
		{
			Device:      config.Device{ID: "m:11000", Host: "m", Port: 11000, Name: "Player One"},
			Name:        "Player One+Player Ü",
			Volume:      31,
			IsGroup:     true,
			TellSlaves:  true,
			NowPlaying:  "Artist - Track",
			Source:      "Spotify",
			Kind:        rowKindGroup,
			Grouping:    "Grouped players: 2",
			GroupDetail: "Overall volume",
		},
		{
			Device:      config.Device{ID: "s:11000", Host: "s", Port: 11000, Name: "Player Ü"},
			Name:        "Player Ü",
			Volume:      29,
			Indent:      3,
			Kind:        rowKindGroupMember,
			Grouping:    "Member of Player One+Player Ü",
			GroupDetail: "Master: Player One",
		},
	}

	cached := cacheRowsFromPlayerRows(rows)
	restored := playerRowsFromCacheRows(cached)
	if len(restored) != 2 {
		t.Fatalf("len(restored)=%d; want 2", len(restored))
	}
	if !restored[0].IsGroup || restored[0].Kind != rowKindGroup {
		t.Fatalf("group row lost: %+v", restored[0])
	}
	if restored[1].Kind != rowKindGroupMember || restored[1].Indent != 3 {
		t.Fatalf("member row lost: %+v", restored[1])
	}
	if restored[1].Name != "Player Ü" {
		t.Fatalf("member name=%q; want Player Ü", restored[1].Name)
	}
}

func TestPlayersTickExpiresTransientStatusAfterTimeout(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := playersModel{
		statusLine: "Muted Player Two",
		statusAt:   now.Add(-(playersStatusTimeout + time.Second)),
	}

	model, _ := m.Update(playersTickMsg(now))
	got, ok := model.(playersModel)
	if !ok {
		t.Fatalf("model type=%T; want playersModel", model)
	}
	if got.statusLine != "" || got.errLine != "" {
		t.Fatalf("status not expired: status=%q err=%q", got.statusLine, got.errLine)
	}
	if !got.statusAt.IsZero() {
		t.Fatalf("statusAt=%v; want zero", got.statusAt)
	}
}

func TestPlayersTickKeepsTransientStatusBeforeTimeout(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := playersModel{
		errLine:  "temporary error",
		statusAt: now.Add(-(playersStatusTimeout - time.Second)),
	}

	model, _ := m.Update(playersTickMsg(now))
	got, ok := model.(playersModel)
	if !ok {
		t.Fatalf("model type=%T; want playersModel", model)
	}
	if got.errLine != "temporary error" {
		t.Fatalf("errLine=%q; want unchanged", got.errLine)
	}
	if got.statusAt.IsZero() {
		t.Fatalf("statusAt cleared too early")
	}
}

func containsControlRunes(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
