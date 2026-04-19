package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/steipete/blucli/internal/bluos"
	"github.com/steipete/blucli/internal/config"
	"github.com/steipete/blucli/internal/discovery"
	"github.com/steipete/blucli/internal/output"
)

const (
	playersRefreshInterval = 4 * time.Second
	playersStatusTimeout   = 20 * time.Second
	playersVolumeStep      = 2
)

var errHDMIARCUnavailable = errors.New("HDMI ARC input is not available")

type playersInputOption struct {
	Label string
	Input string
	URL   string
}

type playerRowKind string

const (
	rowKindStandalone  playerRowKind = "standalone"
	rowKindGroup       playerRowKind = "group"
	rowKindGroupMember playerRowKind = "group_member"
)

type playerRow struct {
	Device        config.Device
	Name          string
	Volume        int
	Mute          bool
	PlaybackState string
	Indent        int
	IsGroup       bool
	TellSlaves    bool
	NowPlaying    string
	Source        string
	Grouping      string
	GroupDetail   string
	StatusDetail  string
	Err           error
	Kind          playerRowKind
}

type playerSnapshot struct {
	Device        config.Device
	Key           string
	Name          string
	Volume        int
	Mute          bool
	PlaybackState string
	StatusText    string
	NowPlaying    string
	Source        string
	Sync          bluos.SyncStatus
	SyncErr       error
	Err           error
}

type playersRefreshedMsg struct {
	Rows []playerRow
	At   time.Time
}

type playersDevicesResolvedMsg struct {
	Devices []config.Device
	Err     error
}

type playersVolumeChangedMsg struct {
	Name     string
	Key      string
	Level    int
	Delta    int
	Member   bool
	Relative bool
	Group    bool
	Err      error
}

type playersMuteChangedMsg struct {
	Name string
	Mute bool
	Err  error
}

type playersPlaybackToggledMsg struct {
	Name    string
	Playing bool
	Err     error
}

type playersGroupChangedMsg struct {
	Status string
	Err    error
}

type playersInputChangedMsg struct {
	Name  string
	Input string
	Err   error
}

type playersInputOptionsLoadedMsg struct {
	DeviceKey string
	Options   []playersInputOption
	Err       error
}

type playersDelayedRefreshMsg struct{}

type playersTickMsg time.Time

type playersModel struct {
	ctx         context.Context
	cfg         config.Config
	cachePath   string
	deviceArg   string
	discover    bool
	discoverTO  time.Duration
	devices     []config.Device
	httpTimeout time.Duration
	dryRun      bool
	trace       io.Writer

	rows                           []playerRow
	selected                       int
	width                          int
	height                         int
	loading                        bool
	lastUpdated                    time.Time
	statusLine                     string
	errLine                        string
	statusAt                       time.Time
	startupStatusUntilFirstRefresh bool

	memberVolumeOverrides map[string]int
	groupPendingKey       string
	inputModalOpen        bool
	inputModalSelected    int
	inputModalOptions     []playersInputOption
	inputModalLoading     bool
	inputModalTargetKey   string
}

type playersViewData struct {
	Width              int
	Height             int
	Loading            bool
	Rows               []playerRow
	Selected           int
	LastUpdated        time.Time
	StatusLine         string
	ErrLine            string
	InputModalOpen     bool
	InputModalSelected int
	InputModalTarget   string
	InputModalOptions  []string
	InputModalLoading  bool
}

func cmdTUI(ctx context.Context, out *output.Printer, cfg config.Config, cache config.DiscoveryCache, cachePath, initialStatusLine, deviceArg string, allowDiscover bool, discoverTimeout, httpTimeout time.Duration, dryRun bool, trace io.Writer, args []string) int {
	if len(args) > 0 {
		out.Errorf("tui: usage: blu tui")
		return 2
	}

	stdoutFile, ok := out.Stdout().(*os.File)
	if !ok || !isTerminalFile(stdoutFile) || !isTerminalFile(os.Stdin) {
		out.Errorf("tui: requires an interactive terminal")
		return 2
	}

	model := newPlayersModel(ctx, cfg, cache, cachePath, initialStatusLine, deviceArg, allowDiscover, discoverTimeout, httpTimeout, dryRun, trace)
	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithOutput(out.Stdout()),
		tea.WithInput(os.Stdin),
	)
	if _, err := p.Run(); err != nil {
		out.Errorf("tui: %v", err)
		return 1
	}
	return 0
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func collectTUIDevices(ctx context.Context, cfg config.Config, cache config.DiscoveryCache, deviceArg string, allowDiscover bool, discoverTimeout time.Duration) ([]config.Device, error) {
	if strings.TrimSpace(deviceArg) != "" {
		device, err := resolveDevice(ctx, cfg, cache, deviceArg, allowDiscover, discoverTimeout)
		if err != nil {
			return nil, err
		}
		return []config.Device{device}, nil
	}

	merged := map[string]config.Device{}
	for _, device := range cache.Devices {
		device.Name = discovery.NormalizeServiceName(device.Name)
		key := deviceKey(device)
		if key == "" {
			continue
		}
		merged[key] = device
	}

	if allowDiscover {
		discoveryCtx, cancel := context.WithTimeout(ctx, discoverTimeout)
		defer cancel()

		discovered, err := discovery.Discover(discoveryCtx)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("discover: %w", err)
		}

		for _, device := range discovered {
			cfgDevice := config.Device{
				ID:   strings.TrimSpace(device.ID),
				Host: strings.TrimSpace(device.Host),
				Port: device.Port,
				Name: discovery.NormalizeServiceName(device.Name),
				Type: strings.TrimSpace(device.Type),
			}
			key := deviceKey(cfgDevice)
			if key == "" {
				continue
			}
			merged[key] = cfgDevice
		}
	}

	if len(merged) == 0 {
		if allowDiscover {
			return nil, errors.New("no devices discovered (run `blu devices` or pass --device)")
		}
		return nil, errors.New("no cached devices (run `blu devices` or pass --device)")
	}

	devices := make([]config.Device, 0, len(merged))
	for _, device := range merged {
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool {
		left := strings.ToLower(displayDeviceName(devices[i]))
		right := strings.ToLower(displayDeviceName(devices[j]))
		if left == right {
			return strings.ToLower(deviceKey(devices[i])) < strings.ToLower(deviceKey(devices[j]))
		}
		return left < right
	})

	return devices, nil
}

func deviceKey(device config.Device) string {
	if strings.TrimSpace(device.ID) != "" {
		return strings.TrimSpace(device.ID)
	}
	if strings.TrimSpace(device.Host) == "" || device.Port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", strings.TrimSpace(device.Host), device.Port)
}

func displayDeviceName(device config.Device) string {
	if strings.TrimSpace(device.Name) != "" {
		return discovery.NormalizeServiceName(device.Name)
	}
	if strings.TrimSpace(device.ID) != "" {
		return strings.TrimSpace(device.ID)
	}
	if strings.TrimSpace(device.Host) != "" && device.Port > 0 {
		return fmt.Sprintf("%s:%d", strings.TrimSpace(device.Host), device.Port)
	}
	return "Unknown player"
}

func newPlayersModel(ctx context.Context, cfg config.Config, cache config.DiscoveryCache, cachePath, initialStatusLine, deviceArg string, allowDiscover bool, discoverTimeout, httpTimeout time.Duration, dryRun bool, trace io.Writer) playersModel {
	rows := playerRowsFromCacheRows(cache.Rows)
	devices := make([]config.Device, 0, len(cache.Devices))
	for _, device := range cache.Devices {
		if deviceKey(device) == "" {
			continue
		}
		device.Name = discovery.NormalizeServiceName(device.Name)
		devices = append(devices, device)
	}
	statusLine := sanitizeTerminalText(initialStatusLine)
	statusAt := time.Time{}
	if statusLine != "" {
		statusAt = time.Now()
	}

	return playersModel{
		ctx:                            ctx,
		cfg:                            cfg,
		cachePath:                      strings.TrimSpace(cachePath),
		deviceArg:                      deviceArg,
		discover:                       allowDiscover,
		discoverTO:                     discoverTimeout,
		devices:                        devices,
		httpTimeout:                    httpTimeout,
		dryRun:                         dryRun,
		trace:                          trace,
		loading:                        true,
		rows:                           rows,
		lastUpdated:                    cache.UpdatedAt,
		statusLine:                     statusLine,
		statusAt:                       statusAt,
		startupStatusUntilFirstRefresh: statusLine != "",
		memberVolumeOverrides:          map[string]int{},
	}
}

func defaultInputModalOptions() []playersInputOption {
	return []playersInputOption{
		{
			Label: "Spotify Connect (spotify open)",
			Input: "Spotify Connect",
			URL:   "Spotify:play",
		},
	}
}

func cloneInputModalOptions(options []playersInputOption) []playersInputOption {
	if len(options) == 0 {
		return nil
	}
	out := make([]playersInputOption, len(options))
	copy(out, options)
	return out
}

func inputModalLabels(options []playersInputOption) []string {
	labels := make([]string, 0, len(options))
	for _, option := range options {
		label := sanitizeTerminalText(option.Label)
		if label == "" {
			continue
		}
		labels = append(labels, label)
	}
	return labels
}

func (m playersModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		playersTick(playersRefreshInterval),
		m.resolveDevicesCmd(),
	}
	if len(m.devices) > 0 {
		cmds = append(cmds, m.refreshCmd())
	}
	return tea.Batch(cmds...)
}

func (m playersModel) resolveDevicesCmd() tea.Cmd {
	ctx := m.ctx
	cfg := m.cfg
	cacheRows := cacheRowsFromPlayerRows(m.rows)
	cache := config.NewDiscoveryCacheWithRows(m.lastUpdated, m.devices, cacheRows)
	deviceArg := m.deviceArg
	allowDiscover := m.discover
	discoverTimeout := m.discoverTO
	return func() tea.Msg {
		devices, err := collectTUIDevices(ctx, cfg, cache, deviceArg, allowDiscover, discoverTimeout)
		return playersDevicesResolvedMsg{Devices: devices, Err: err}
	}
}

func (m playersModel) refreshCmd() tea.Cmd {
	ctx := m.ctx
	devices := append([]config.Device(nil), m.devices...)
	if len(devices) == 0 {
		return nil
	}
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace

	return func() tea.Msg {
		rows := fetchPlayerRows(ctx, devices, httpTimeout, dryRun, trace)
		return playersRefreshedMsg{Rows: rows, At: time.Now()}
	}
}

func (m playersModel) saveCacheCmd(at time.Time) tea.Cmd {
	cachePath := strings.TrimSpace(m.cachePath)
	if cachePath == "" {
		return nil
	}
	devices := append([]config.Device(nil), m.devices...)
	rows := cacheRowsFromPlayerRows(m.rows)
	return func() tea.Msg {
		cache := config.NewDiscoveryCacheWithRows(at, devices, rows)
		_ = config.SaveDiscoveryCache(cachePath, cache)
		return nil
	}
}

func cacheRowsFromPlayerRows(rows []playerRow) []config.CachedPlayerRow {
	out := make([]config.CachedPlayerRow, 0, len(rows))
	for _, row := range rows {
		if deviceKey(row.Device) == "" {
			continue
		}
		out = append(out, config.CachedPlayerRow{
			Device:        row.Device,
			Name:          row.Name,
			Volume:        row.Volume,
			Mute:          row.Mute,
			PlaybackState: row.PlaybackState,
			Indent:        row.Indent,
			IsGroup:       row.IsGroup,
			TellSlaves:    row.TellSlaves,
			NowPlaying:    row.NowPlaying,
			Source:        row.Source,
			Grouping:      row.Grouping,
			GroupDetail:   row.GroupDetail,
			StatusDetail:  row.StatusDetail,
			Kind:          string(row.Kind),
		})
	}
	return out
}

func playerRowsFromCacheRows(rows []config.CachedPlayerRow) []playerRow {
	out := make([]playerRow, 0, len(rows))
	for _, row := range rows {
		if deviceKey(row.Device) == "" {
			continue
		}
		kind := playerRowKind(strings.TrimSpace(row.Kind))
		switch kind {
		case rowKindStandalone, rowKindGroup, rowKindGroupMember:
		default:
			if row.IsGroup {
				kind = rowKindGroup
			} else if row.Indent > 0 {
				kind = rowKindGroupMember
			} else {
				kind = rowKindStandalone
			}
		}
		name := sanitizeTerminalText(row.Name)
		if name == "" {
			name = displayDeviceName(row.Device)
		}
		out = append(out, playerRow{
			Device:        row.Device,
			Name:          name,
			Volume:        clampInt(row.Volume, 0, 100),
			Mute:          row.Mute,
			PlaybackState: row.PlaybackState,
			Indent:        maxInt(0, row.Indent),
			IsGroup:       row.IsGroup,
			TellSlaves:    row.TellSlaves,
			NowPlaying:    sanitizeTerminalText(row.NowPlaying),
			Source:        sanitizeTerminalText(row.Source),
			Grouping:      sanitizeTerminalText(row.Grouping),
			GroupDetail:   sanitizeTerminalText(row.GroupDetail),
			StatusDetail:  sanitizeTerminalText(row.StatusDetail),
			Kind:          kind,
		})
	}
	return out
}

func (m playersModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case playersDevicesResolvedMsg:
		m.loading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
			return m, nil
		}
		m.devices = msg.Devices
		m.setErrLine("")
		m.loading = true
		return m, m.refreshCmd()
	case playersTickMsg:
		m.maybeExpireStatus(time.Time(msg))
		if len(m.devices) == 0 {
			return m, playersTick(playersRefreshInterval)
		}
		m.loading = true
		return m, tea.Batch(m.refreshCmd(), playersTick(playersRefreshInterval))
	case playersDelayedRefreshMsg:
		if len(m.devices) == 0 {
			return m, nil
		}
		m.loading = true
		return m, m.refreshCmd()
	case playersRefreshedMsg:
		m.loading = false
		m.rows = msg.Rows
		m.applyMemberVolumeOverrides()
		if m.selected >= len(m.rows) {
			m.selected = maxInt(0, len(m.rows)-1)
		}
		m.lastUpdated = msg.At
		m.maybeExpireStatus(msg.At)
		m.setErrLine("")
		if m.startupStatusUntilFirstRefresh {
			m.statusLine = ""
			m.statusAt = time.Time{}
			m.startupStatusUntilFirstRefresh = false
		}
		return m, m.saveCacheCmd(msg.At)
	case playersVolumeChangedMsg:
		m.loading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
			return m, nil
		}
		m.applyVolumeChange(msg)
		if msg.Member && msg.Key != "" {
			m.memberVolumeOverrides[msg.Key] = clampInt(msg.Level, 0, 100)
		}
		if msg.Relative {
			if msg.Group {
				m.setStatusLine(fmt.Sprintf("Adjusted group volume for %s", msg.Name))
			} else {
				m.setStatusLine(fmt.Sprintf("Adjusted volume for %s", msg.Name))
			}
		} else {
			m.setStatusLine(fmt.Sprintf("Updated %s to %d%%", msg.Name, msg.Level))
		}
		m.loading = true
		return m, playersRefreshAfter(650 * time.Millisecond)
	case playersMuteChangedMsg:
		m.loading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
			return m, nil
		}
		if msg.Mute {
			m.setStatusLine(fmt.Sprintf("Muted %s", msg.Name))
		} else {
			m.setStatusLine(fmt.Sprintf("Unmuted %s", msg.Name))
		}
		m.loading = true
		return m, m.refreshCmd()
	case playersPlaybackToggledMsg:
		m.loading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
			return m, nil
		}
		if msg.Playing {
			m.setStatusLine(fmt.Sprintf("Playing %s", msg.Name))
		} else {
			m.setStatusLine(fmt.Sprintf("Paused %s", msg.Name))
		}
		m.loading = true
		return m, m.refreshCmd()
	case playersGroupChangedMsg:
		m.loading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
			return m, nil
		}
		if strings.TrimSpace(msg.Status) != "" {
			m.setStatusLine(msg.Status)
		}
		m.loading = true
		return m, m.refreshCmd()
	case playersInputChangedMsg:
		m.loading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
			return m, nil
		}
		if strings.TrimSpace(msg.Name) != "" {
			m.setStatusLine(fmt.Sprintf("Switched %s to %s", msg.Name, strings.TrimSpace(msg.Input)))
		} else {
			m.setStatusLine(fmt.Sprintf("Switched input to %s", strings.TrimSpace(msg.Input)))
		}
		m.loading = true
		return m, m.refreshCmd()
	case playersInputOptionsLoadedMsg:
		if !m.inputModalOpen {
			return m, nil
		}
		if msg.DeviceKey != m.inputModalTargetKey {
			return m, nil
		}
		m.inputModalLoading = false
		if msg.Err != nil {
			m.setErrLine(msg.Err.Error())
		}
		if len(msg.Options) > 0 {
			m.inputModalOptions = cloneInputModalOptions(msg.Options)
		}
		maxIndex := len(m.currentInputModalOptions()) - 1
		if m.inputModalSelected > maxIndex {
			m.inputModalSelected = maxIndex
		}
		if m.inputModalSelected < 0 {
			m.inputModalSelected = 0
		}
		return m, nil
	case tea.KeyMsg:
		if m.inputModalOpen {
			options := m.currentInputModalOptions()
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc", "q", ":":
				m.inputModalOpen = false
				m.inputModalLoading = false
				m.inputModalTargetKey = ""
				return m, nil
			case "up":
				if m.inputModalSelected > 0 {
					m.inputModalSelected--
				}
				return m, nil
			case "down":
				if m.inputModalSelected+1 < len(options) {
					m.inputModalSelected++
				}
				return m, nil
			case "enter", " ", "space":
				if len(m.rows) == 0 || m.selected < 0 || m.selected >= len(m.rows) {
					m.inputModalOpen = false
					m.inputModalLoading = false
					m.inputModalTargetKey = ""
					m.setErrLine("cannot switch input: no player selected")
					return m, nil
				}
				if len(options) == 0 || m.inputModalSelected < 0 || m.inputModalSelected >= len(options) {
					m.inputModalOpen = false
					m.inputModalLoading = false
					m.inputModalTargetKey = ""
					m.setErrLine("cannot switch input: no inputs available")
					return m, nil
				}
				row := m.rows[m.selected]
				option := options[m.inputModalSelected]
				m.inputModalOpen = false
				m.inputModalLoading = false
				m.inputModalTargetKey = ""
				m.loading = true
				return m, m.changeInputCmd(row, option)
			default:
				return m, nil
			}
		}

		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up":
			if m.selected > 0 {
				m.selected--
			}
			return m, nil
		case "down":
			if m.selected+1 < len(m.rows) {
				m.selected++
			}
			return m, nil
		case "r":
			m.loading = true
			return m, m.resolveDevicesCmd()
		case ":":
			if len(m.rows) == 0 || m.selected < 0 || m.selected >= len(m.rows) {
				m.setErrLine("cannot open input menu: no player selected")
				return m, nil
			}
			row := m.rows[m.selected]
			targetKey := deviceKey(row.Device)
			options := defaultInputModalOptions()
			m.inputModalOpen = true
			m.inputModalSelected = 0
			m.inputModalOptions = cloneInputModalOptions(options)
			m.inputModalLoading = true
			m.inputModalTargetKey = targetKey
			return m, m.loadInputModalOptionsCmd(row, targetKey, options)
		case "right", "l":
			if len(m.rows) == 0 {
				return m, nil
			}
			m.loading = true
			return m, m.adjustVolumeCmd(playersVolumeStep)
		case "left", "h":
			if len(m.rows) == 0 {
				return m, nil
			}
			m.loading = true
			return m, m.adjustVolumeCmd(-playersVolumeStep)
		case "m":
			if len(m.rows) == 0 {
				return m, nil
			}
			m.loading = true
			return m, m.toggleMuteCmd()
		case " ", "space":
			if len(m.rows) == 0 {
				return m, nil
			}
			m.loading = true
			return m, m.togglePlaybackCmd()
		case "g":
			if len(m.rows) == 0 || m.selected < 0 || m.selected >= len(m.rows) {
				return m, nil
			}
			row := m.rows[m.selected]

			// Ungroup selected grouped member immediately.
			if isGroupedMemberRow(row) {
				m.groupPendingKey = ""
				m.loading = true
				return m, m.ungroupSelectedMemberCmd()
			}

			// First press: remember selected standalone player or group.
			if m.groupPendingKey == "" {
				key := deviceKey(row.Device)
				if key == "" {
					m.setErrLine(fmt.Sprintf("cannot select %s for grouping: missing device key", row.Name))
					return m, nil
				}
				m.groupPendingKey = key
				if row.IsGroup {
					m.setStatusLine(fmt.Sprintf("Selected group %s. Press g on player to add", row.Name))
				} else {
					m.setStatusLine(fmt.Sprintf("Selected %s for grouping. Press g on player/group to apply", row.Name))
				}
				return m, nil
			}

			// Second press on same player: clear pending selection.
			if deviceKey(row.Device) == m.groupPendingKey {
				m.groupPendingKey = ""
				m.setStatusLine("Grouping selection cleared")
				return m, nil
			}

			pendingRow, _, ok := findRowByKey(m.rows, m.groupPendingKey)
			if !ok {
				m.groupPendingKey = ""
				m.setErrLine("pending grouping player is no longer visible; selection cleared")
				return m, nil
			}
			if isGroupedMemberRow(pendingRow) {
				m.groupPendingKey = ""
				m.setErrLine("pending player is no longer eligible for grouping; selection cleared")
				return m, nil
			}

			m.groupPendingKey = ""
			m.loading = true

			// Apply selection:
			// - pending group + selected standalone row: add selected player to pending group
			if pendingRow.IsGroup {
				if row.IsGroup {
					m.loading = false
					m.setStatusLine("Select an individual player to add to the remembered group")
					return m, nil
				}
				return m, m.addSlaveToGroupCmd(pendingRow, row, fmt.Sprintf("Added %s to %s", row.Name, pendingRow.Name))
			}

			// - pending standalone row + selected group row: add pending player to selected group
			// - on group row: add pending player to this group
			// - on standalone row: create group (pending as master, selected as slave)
			if row.IsGroup {
				return m, m.addSlaveToGroupCmd(row, pendingRow, fmt.Sprintf("Added %s to %s", pendingRow.Name, row.Name))
			}
			return m, m.addSlaveToGroupCmd(pendingRow, row, fmt.Sprintf("Grouped %s with %s", row.Name, pendingRow.Name))
		}
	}
	return m, nil
}

func (m playersModel) View() string {
	inputTarget := ""
	if m.inputModalOpen && m.selected >= 0 && m.selected < len(m.rows) {
		inputTarget = sanitizeTerminalText(m.rows[m.selected].Name)
	}
	inputOptions := inputModalLabels(m.currentInputModalOptions())
	return renderPlayersView(playersViewData{
		Width:              m.width,
		Height:             m.height,
		Loading:            m.loading,
		Rows:               m.rows,
		Selected:           m.selected,
		LastUpdated:        m.lastUpdated,
		StatusLine:         m.statusLine,
		ErrLine:            m.errLine,
		InputModalOpen:     m.inputModalOpen,
		InputModalSelected: m.inputModalSelected,
		InputModalTarget:   inputTarget,
		InputModalOptions:  inputOptions,
		InputModalLoading:  m.inputModalLoading,
	})
}

func renderPlayersView(data playersViewData) string {
	if data.Width <= 0 || data.Height <= 0 {
		return "Loading..."
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	panelStyle := lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("8")).Padding(0, 1)

	bodyHeight := data.Height - 5
	if bodyHeight < 8 {
		return dimStyle.Render("Enlarge terminal window to render Players view.")
	}

	// Keep a small right gutter to avoid terminal-edge clipping of the border.
	panelWidth := maxInt(24, data.Width-2)
	topLine := dimStyle.Render(formatHeadlineLine("", "", "Blue Sound", panelWidth))
	initialLoad := data.Loading && data.LastUpdated.IsZero()
	leftLines := renderPlayersBodyLines(data.Rows, data.Selected, panelWidth, bodyHeight, selectedStyle, initialLoad)
	if data.InputModalOpen {
		leftLines = overlayInputModal(leftLines, maxInt(8, panelWidth-4), data.InputModalTarget, data.InputModalOptions, data.InputModalSelected, data.InputModalLoading)
	}
	content := panelStyle.Width(panelWidth).Height(bodyHeight).Render(strings.Join(leftLines, "\n"))

	hints := dimStyle.Render("Keys: : input  space play/pause  m mute  g group/ungroup  r refresh  q quit")
	statusText := ""
	statusStyle := dimStyle
	if data.ErrLine != "" {
		statusText = data.ErrLine
		statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	} else if data.StatusLine != "" {
		statusText = data.StatusLine
	} else if initialLoad {
		statusText = "Loading players..."
	} else if !data.LastUpdated.IsZero() {
		statusText = "Last update: " + data.LastUpdated.Format("15:04:05")
	}
	menu := formatFooterBar(panelWidth, "Players", selectedStyle, statusText, statusStyle)

	return lipgloss.JoinVertical(
		lipgloss.Left,
		topLine,
		content,
		menu,
		hints,
	)
}

func renderPlayersBodyLines(rows []playerRow, selected, panelWidth, bodyHeight int, selectedStyle lipgloss.Style, initialLoad bool) []string {
	const rowBlockHeight = 4 // title + volume + two spacer rows
	rowsPerPage := maxInt(1, (bodyHeight-2)/rowBlockHeight)
	start, end := visibleWindow(len(rows), selected, rowsPerPage)

	leftLines := make([]string, 0, rowsPerPage*rowBlockHeight)
	memberNameColWidth := groupedMemberNameColumnWidth(rows, start, end, panelWidth)

	if len(rows) == 0 {
		if initialLoad {
			leftLines = append(leftLines, "Loading players...")
			leftLines = append(leftLines, "Collecting volume, playback and grouping data.")
			return leftLines
		}
		leftLines = append(leftLines, "No players found.")
		leftLines = append(leftLines, "Run `blu devices` or enable discovery.")
		return leftLines
	}

	for i := start; i < end; i++ {
		row := rows[i]
		isSelected := i == selected
		compactMember := isGroupedMemberRow(row)
		marker := "  "
		if isSelected {
			marker = "> "
		}

		displayName := sanitizeTerminalText(row.Name)
		if row.IsGroup {
			displayName = "Group: " + displayName
		}
		displayName = strings.Repeat(" ", row.Indent) + displayName
		rightText := sanitizeTerminalText(row.NowPlaying)
		source := sanitizeTerminalText(row.Source)
		if rightText != "" && source != "" {
			rightText = source + ": " + rightText
		}
		if compactMember {
			compactRight := fmt.Sprintf("%d%%", clampInt(row.Volume, 0, 100))
			if row.Mute {
				compactRight = "muted"
			}
			if row.Err != nil {
				compactRight = "ERR"
			}
			line := formatCompactMemberLine(marker, displayName, compactRight, maxInt(8, panelWidth-4), memberNameColWidth)
			if isSelected {
				line = selectedStyle.Render(line)
			}
			leftLines = append(leftLines, line)
		} else {
			leftTop := formatHeadlineLine(marker, displayName, rightText, maxInt(8, panelWidth-4))
			leftBottom := "  " + strings.Repeat(" ", row.Indent)
			if row.Err != nil {
				leftBottom += "ERR " + truncateText(sanitizeTerminalText(row.Err.Error()), maxInt(4, panelWidth-6))
			} else {
				vol := fmt.Sprintf("%d%%", clampInt(row.Volume, 0, 100))
				if row.Mute {
					vol = "muted"
				}
				barWidth := maxInt(8, panelWidth-12-row.Indent)
				leftBottom += fmt.Sprintf("%5s %s", vol, volumeBar(row.Volume, barWidth))
			}

			if isSelected {
				leftTop = selectedStyle.Render(leftTop)
				leftBottom = selectedStyle.Render(leftBottom)
			}
			leftLines = append(leftLines, leftTop, leftBottom)
		}
		if i+1 < end {
			for range spacingLinesBetweenRows(row, rows[i+1]) {
				leftLines = append(leftLines, "")
			}
		}
	}
	return leftLines
}

func overlayInputModal(baseLines []string, contentWidth int, target string, options []string, selected int, loading bool) []string {
	if contentWidth < 20 {
		return baseLines
	}
	lines := append([]string(nil), baseLines...)
	if len(lines) == 0 {
		lines = []string{""}
	}
	for i := range lines {
		w := textWidth(lines[i])
		if w < contentWidth {
			lines[i] += strings.Repeat(" ", contentWidth-w)
		} else if w > contentWidth {
			lines[i] = truncateTextPreserveLeft(lines[i], contentWidth)
		}
	}

	title := "Select Input"
	target = sanitizeTerminalText(target)
	if target != "" {
		title = "Select Input: " + target
	}
	opts := make([]string, 0, len(options)+3)
	opts = append(opts, title)
	if loading {
		opts = append(opts, "  Loading available inputs...")
	}
	for i, option := range options {
		option = sanitizeTerminalText(option)
		prefix := "  "
		if i == selected {
			prefix = "> "
		}
		opts = append(opts, prefix+option)
	}
	if len(options) == 0 {
		opts = append(opts, "  No inputs available")
	}
	opts = append(opts, "Esc: close  Enter: apply")

	maxLen := 0
	for _, line := range opts {
		if w := textWidth(line); w > maxLen {
			maxLen = w
		}
	}
	maxModalWidth := maxInt(20, contentWidth-4)
	modalWidth := clampInt(maxLen+2, 20, maxModalWidth)
	modalContent := make([]string, 0, len(opts))
	for _, line := range opts {
		modalContent = append(modalContent, formatHeadlineLine("", line, "", modalWidth))
	}

	modalStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("8")).
		Padding(0, 1)
	modal := modalStyle.Render(strings.Join(modalContent, "\n"))
	modalLines := strings.Split(modal, "\n")

	startY := maxInt(0, (len(lines)-len(modalLines))/2)
	for i := 0; i < len(modalLines) && startY+i < len(lines); i++ {
		line := modalLines[i]
		startX := maxInt(0, (contentWidth-textWidth(line))/2)
		placed := strings.Repeat(" ", startX) + line
		w := textWidth(placed)
		if w < contentWidth {
			placed += strings.Repeat(" ", contentWidth-w)
		} else if w > contentWidth {
			placed = truncateTextPreserveLeft(placed, contentWidth)
		}
		lines[startY+i] = placed
	}

	return lines
}

func (m playersModel) adjustVolumeCmd(delta int) tea.Cmd {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return nil
	}

	row := m.rows[m.selected]
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	return func() tea.Msg {
		if row.Err != nil {
			return playersVolumeChangedMsg{Err: fmt.Errorf("cannot adjust volume for %s: %v", row.Name, row.Err)}
		}
		client := bluos.NewClient(row.Device.BaseURL(), bluos.Options{Timeout: httpTimeout, DryRun: dryRun, Trace: trace})
		target := clampInt(row.Volume+delta, 0, 100)
		// Use relative dB changes for both paths:
		// - group rows with tell_slaves=1 (proportional group change)
		// - member/standalone rows with tell_slaves=0 (individual change)
		err := client.VolumeDeltaDB(ctx, bluos.VolumeDeltaDBOptions{DeltaDB: delta, TellSlaves: row.IsGroup})
		if errors.Is(err, bluos.ErrDryRun) {
			err = nil
		}
		return playersVolumeChangedMsg{
			Name:     row.Name,
			Key:      deviceKey(row.Device),
			Level:    target,
			Delta:    delta,
			Member:   row.Kind == rowKindGroupMember,
			Relative: true,
			Group:    row.IsGroup,
			Err:      err,
		}
	}
}

func (m playersModel) toggleMuteCmd() tea.Cmd {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return nil
	}

	row := m.rows[m.selected]
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	return func() tea.Msg {
		if row.Err != nil {
			return playersMuteChangedMsg{Err: fmt.Errorf("cannot toggle mute for %s: %v", row.Name, row.Err)}
		}
		target := !row.Mute
		client := bluos.NewClient(row.Device.BaseURL(), bluos.Options{Timeout: httpTimeout, DryRun: dryRun, Trace: trace})
		err := client.VolumeMute(ctx, bluos.VolumeMuteOptions{Mute: target, TellSlaves: row.TellSlaves})
		if errors.Is(err, bluos.ErrDryRun) {
			err = nil
		}
		return playersMuteChangedMsg{Name: row.Name, Mute: target, Err: err}
	}
}

func (m playersModel) togglePlaybackCmd() tea.Cmd {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return nil
	}

	row := m.rows[m.selected]
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	return func() tea.Msg {
		if row.Err != nil {
			return playersPlaybackToggledMsg{Err: fmt.Errorf("cannot toggle playback for %s: %v", row.Name, row.Err)}
		}

		action := playbackToggleAction(row.PlaybackState)
		client := bluos.NewClient(row.Device.BaseURL(), bluos.Options{Timeout: httpTimeout, DryRun: dryRun, Trace: trace})
		var err error
		play := false
		switch action {
		case "pause":
			err = client.Pause(ctx, bluos.PauseOptions{})
			play = false
		default:
			err = client.Play(ctx, bluos.PlayOptions{})
			play = true
		}
		if errors.Is(err, bluos.ErrDryRun) {
			err = nil
		}
		return playersPlaybackToggledMsg{Name: row.Name, Playing: play, Err: err}
	}
}

func (m playersModel) currentInputModalOptions() []playersInputOption {
	if len(m.inputModalOptions) == 0 {
		return defaultInputModalOptions()
	}
	return m.inputModalOptions
}

func (m playersModel) loadInputModalOptionsCmd(row playerRow, deviceKey string, base []playersInputOption) tea.Cmd {
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	baseOptions := cloneInputModalOptions(base)
	return func() tea.Msg {
		options := cloneInputModalOptions(baseOptions)
		client := bluos.NewClient(row.Device.BaseURL(), bluos.Options{Timeout: httpTimeout, DryRun: dryRun, Trace: trace})
		hdmiURL, err := resolveHDMIARCInputURL(ctx, client)
		if err == nil {
			options = append(options, playersInputOption{
				Label: "HDMI ARC",
				Input: "HDMI ARC",
				URL:   hdmiURL,
			})
		} else if !errors.Is(err, errHDMIARCUnavailable) {
			return playersInputOptionsLoadedMsg{
				DeviceKey: deviceKey,
				Options:   options,
				Err:       fmt.Errorf("cannot load HDMI ARC for %s: %v", row.Name, err),
			}
		}

		return playersInputOptionsLoadedMsg{
			DeviceKey: deviceKey,
			Options:   options,
		}
	}
}

func (m playersModel) changeInputCmd(row playerRow, option playersInputOption) tea.Cmd {
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	return func() tea.Msg {
		if row.Err != nil {
			return playersInputChangedMsg{Err: fmt.Errorf("cannot switch input for %s: %v", row.Name, row.Err)}
		}
		if strings.TrimSpace(option.URL) == "" {
			return playersInputChangedMsg{Err: errors.New("invalid input option")}
		}

		client := bluos.NewClient(row.Device.BaseURL(), bluos.Options{Timeout: httpTimeout, DryRun: dryRun, Trace: trace})
		err := client.Play(ctx, bluos.PlayOptions{URL: option.URL})
		if errors.Is(err, bluos.ErrDryRun) {
			err = nil
		}

		input := strings.TrimSpace(option.Input)
		if input == "" {
			input = strings.TrimSpace(option.Label)
		}
		return playersInputChangedMsg{Name: row.Name, Input: input, Err: err}
	}
}

func resolveHDMIARCInputURL(ctx context.Context, client *bluos.Client) (string, error) {
	rb, err := client.RadioBrowse(ctx, bluos.RadioBrowseOptions{Service: "Capture"})
	if err != nil {
		return "", fmt.Errorf("capture inputs: %w", err)
	}
	for _, item := range rb.Items {
		if !looksLikeHDMIARCInput(item) {
			continue
		}
		raw := strings.TrimSpace(item.URL)
		if raw == "" {
			continue
		}
		if decoded, decodeErr := url.QueryUnescape(raw); decodeErr == nil {
			raw = decoded
		}
		return raw, nil
	}
	return "", errHDMIARCUnavailable
}

func looksLikeHDMIARCInput(item bluos.RadioItem) bool {
	fields := []string{item.ID, item.Text, item.Type, item.InputType, item.URL}
	for _, field := range fields {
		value := strings.ToLower(strings.TrimSpace(field))
		if strings.Contains(value, "hdmi") || strings.Contains(value, "earc") {
			return true
		}
	}
	return false
}

func (m playersModel) ungroupSelectedMemberCmd() tea.Cmd {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return nil
	}
	member := m.rows[m.selected]
	master, ok := findMasterRowForMember(m.rows, m.selected)
	if !ok {
		return func() tea.Msg {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot ungroup %s: missing group master", member.Name)}
		}
	}
	return m.removeSlaveFromGroupCmd(master, member, fmt.Sprintf("Ungrouped %s", member.Name))
}

func (m playersModel) addSlaveToGroupCmd(master, slave playerRow, status string) tea.Cmd {
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	return func() tea.Msg {
		if master.Err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot modify group %s: %v", master.Name, master.Err)}
		}
		if slave.Err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot group %s: %v", slave.Name, slave.Err)}
		}

		masterHost, masterPort, err := deviceHostPort(master.Device)
		if err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot resolve master %s: %v", master.Name, err)}
		}
		slaveHost, slavePort, err := deviceHostPort(slave.Device)
		if err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot resolve slave %s: %v", slave.Name, err)}
		}
		if canonicalHostPortKey(masterHost, masterPort) == canonicalHostPortKey(slaveHost, slavePort) {
			return playersGroupChangedMsg{Err: errors.New("cannot group a player with itself")}
		}

		client := bluos.NewClient((&config.Device{Host: masterHost, Port: masterPort}).BaseURL(), bluos.Options{
			Timeout: httpTimeout,
			DryRun:  dryRun,
			Trace:   trace,
		})
		err = client.AddSlave(ctx, bluos.AddSlaveOptions{SlaveHost: slaveHost, SlavePort: slavePort})
		if errors.Is(err, bluos.ErrDryRun) {
			err = nil
		}
		return playersGroupChangedMsg{Status: status, Err: err}
	}
}

func (m playersModel) removeSlaveFromGroupCmd(master, slave playerRow, status string) tea.Cmd {
	httpTimeout := m.httpTimeout
	dryRun := m.dryRun
	trace := m.trace
	ctx := m.ctx

	return func() tea.Msg {
		if master.Err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot modify group %s: %v", master.Name, master.Err)}
		}
		if slave.Err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot ungroup %s: %v", slave.Name, slave.Err)}
		}

		masterHost, masterPort, err := deviceHostPort(master.Device)
		if err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot resolve master %s: %v", master.Name, err)}
		}
		slaveHost, slavePort, err := deviceHostPort(slave.Device)
		if err != nil {
			return playersGroupChangedMsg{Err: fmt.Errorf("cannot resolve slave %s: %v", slave.Name, err)}
		}
		if canonicalHostPortKey(masterHost, masterPort) == canonicalHostPortKey(slaveHost, slavePort) {
			return playersGroupChangedMsg{Err: errors.New("cannot remove master from itself")}
		}

		client := bluos.NewClient((&config.Device{Host: masterHost, Port: masterPort}).BaseURL(), bluos.Options{
			Timeout: httpTimeout,
			DryRun:  dryRun,
			Trace:   trace,
		})
		err = client.RemoveSlave(ctx, bluos.RemoveSlaveOptions{SlaveHost: slaveHost, SlavePort: slavePort})
		if errors.Is(err, bluos.ErrDryRun) {
			err = nil
		}
		return playersGroupChangedMsg{Status: status, Err: err}
	}
}

func playersTick(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return playersTickMsg(t)
	})
}

func playersRefreshAfter(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return playersDelayedRefreshMsg{}
	})
}

func (m *playersModel) setStatusLine(text string) {
	text = sanitizeTerminalText(text)
	m.statusLine = text
	m.errLine = ""
	if text == "" {
		if strings.TrimSpace(m.errLine) == "" {
			m.statusAt = time.Time{}
		}
		return
	}
	m.statusAt = time.Now()
}

func (m *playersModel) setErrLine(text string) {
	text = sanitizeTerminalText(text)
	m.errLine = text
	if text == "" {
		if strings.TrimSpace(m.statusLine) == "" {
			m.statusAt = time.Time{}
		}
		return
	}
	m.statusLine = ""
	m.statusAt = time.Now()
}

func (m *playersModel) maybeExpireStatus(now time.Time) {
	if m.statusAt.IsZero() {
		return
	}
	if now.Sub(m.statusAt) < playersStatusTimeout {
		return
	}
	m.statusLine = ""
	m.errLine = ""
	m.statusAt = time.Time{}
}

func (m *playersModel) applyVolumeChange(msg playersVolumeChangedMsg) {
	if msg.Key == "" || len(m.rows) == 0 {
		return
	}

	if msg.Group {
		for i := range m.rows {
			if m.rows[i].Kind != rowKindGroup {
				continue
			}
			if deviceKey(m.rows[i].Device) != msg.Key {
				continue
			}
			m.rows[i].Volume = clampInt(m.rows[i].Volume+msg.Delta, 0, 100)
			for j := i + 1; j < len(m.rows); j++ {
				if m.rows[j].Kind != rowKindGroupMember {
					break
				}
				m.rows[j].Volume = clampInt(m.rows[j].Volume+msg.Delta, 0, 100)
				if memberKey := deviceKey(m.rows[j].Device); memberKey != "" {
					if existing, ok := m.memberVolumeOverrides[memberKey]; ok {
						m.memberVolumeOverrides[memberKey] = clampInt(existing+msg.Delta, 0, 100)
					}
				}
			}
			return
		}
		return
	}

	for i := range m.rows {
		if m.rows[i].Kind == rowKindGroup {
			continue
		}
		if deviceKey(m.rows[i].Device) == msg.Key {
			m.rows[i].Volume = clampInt(msg.Level, 0, 100)
			return
		}
	}
}

func (m *playersModel) applyMemberVolumeOverrides() {
	if len(m.memberVolumeOverrides) == 0 || len(m.rows) == 0 {
		return
	}

	currentMembers := map[string]struct{}{}
	for i := range m.rows {
		if m.rows[i].Kind != rowKindGroupMember {
			continue
		}
		key := deviceKey(m.rows[i].Device)
		if key == "" {
			continue
		}
		currentMembers[key] = struct{}{}
		if level, ok := m.memberVolumeOverrides[key]; ok {
			m.rows[i].Volume = clampInt(level, 0, 100)
		}
	}

	for key := range m.memberVolumeOverrides {
		if _, ok := currentMembers[key]; !ok {
			delete(m.memberVolumeOverrides, key)
		}
	}
}

func fetchPlayerRows(ctx context.Context, devices []config.Device, httpTimeout time.Duration, dryRun bool, trace io.Writer) []playerRow {
	snapshots := make([]playerSnapshot, len(devices))

	var wg sync.WaitGroup
	wg.Add(len(devices))
	for i, device := range devices {
		go func(i int, device config.Device) {
			defer wg.Done()

			client := bluos.NewClient(device.BaseURL(), bluos.Options{Timeout: httpTimeout, DryRun: dryRun, Trace: trace})
			status, statusErr := client.Status(ctx, bluos.StatusOptions{})
			syncStatus, syncErr := client.SyncStatus(ctx, bluos.SyncStatusOptions{})

			name := sanitizeTerminalText(discovery.NormalizeServiceName(status.Name))
			if name == "" {
				name = displayDeviceName(device)
			}

			snapshot := playerSnapshot{
				Device:        device,
				Key:           canonicalHostPortKey(device.Host, device.Port),
				Name:          name,
				Volume:        clampInt(status.Volume, 0, 100),
				Mute:          bool(status.Mute),
				PlaybackState: normalizePlaybackState(status.State),
				StatusText:    sanitizeTerminalText(summarizeStatus(status)),
				NowPlaying:    sanitizeTerminalText(nowPlayingText(status)),
				Source:        sanitizeTerminalText(status.Source),
				Sync:          syncStatus,
				SyncErr:       syncErr,
			}
			if snapshot.Key == "" {
				snapshot.Key = deviceKey(device)
			}
			if statusErr != nil {
				snapshot.Err = sanitizeTerminalError(fmt.Errorf("status: %w", statusErr))
				snapshot.Volume = 0
				snapshot.Mute = false
				if syncErr != nil {
					snapshot.Err = sanitizeTerminalError(fmt.Errorf("status: %v; syncstatus: %v", statusErr, syncErr))
				}
			}
			snapshots[i] = snapshot
		}(i, device)
	}
	wg.Wait()

	return buildRowsFromSnapshots(snapshots)
}

func buildRowsFromSnapshots(snapshots []playerSnapshot) []playerRow {
	if len(snapshots) == 0 {
		return nil
	}

	byKey := make(map[string]playerSnapshot, len(snapshots))
	byHost := map[string][]string{}
	membersByLeader := map[string][]string{}
	groupedByLeader := map[string]bool{}
	groupNameByLeader := map[string]string{}

	for _, snapshot := range snapshots {
		if snapshot.Key == "" {
			continue
		}
		byKey[snapshot.Key] = snapshot
		if host := canonicalHostKey(snapshot.Device.Host); host != "" {
			byHost[host] = append(byHost[host], snapshot.Key)
		}

		leader := snapshot.Key
		if snapshot.SyncErr == nil && snapshot.Sync.Master != nil {
			if masterKey := canonicalHostPortKey(snapshot.Sync.Master.Host, snapshot.Sync.Master.Port); masterKey != "" {
				leader = masterKey
			}
		}

		membersByLeader[leader] = append(membersByLeader[leader], snapshot.Key)
		if snapshot.SyncErr == nil {
			if snapshot.Sync.Master != nil || len(snapshot.Sync.Slaves) > 0 {
				groupedByLeader[leader] = true
			}
			if group := sanitizeTerminalText(snapshot.Sync.Group); group != "" && groupNameByLeader[leader] == "" {
				groupNameByLeader[leader] = group
			}
		}
	}

	leaderKeys := make([]string, 0, len(membersByLeader))
	for leader := range membersByLeader {
		leaderKeys = append(leaderKeys, leader)
	}
	sort.Slice(leaderKeys, func(i, j int) bool {
		left := strings.ToLower(leaderDisplayName(leaderKeys[i], byKey, membersByLeader, groupNameByLeader))
		right := strings.ToLower(leaderDisplayName(leaderKeys[j], byKey, membersByLeader, groupNameByLeader))
		if left == right {
			return strings.ToLower(leaderKeys[i]) < strings.ToLower(leaderKeys[j])
		}
		return left < right
	})

	rows := make([]playerRow, 0, len(snapshots)*2)
	for _, leader := range leaderKeys {
		memberKeys := uniqueStrings(membersByLeader[leader])
		sort.Slice(memberKeys, func(i, j int) bool {
			left := leaderMemberName(memberKeys[i], byKey)
			right := leaderMemberName(memberKeys[j], byKey)
			if left == right {
				return memberKeys[i] < memberKeys[j]
			}
			return left < right
		})

		leaderSnapshot, hasLeader := byKey[leader]
		grouped := groupedByLeader[leader] || len(memberKeys) > 1
		groupName := sanitizeTerminalText(groupNameByLeader[leader])
		if groupName == "" && hasLeader {
			groupName = sanitizeTerminalText(leaderSnapshot.Sync.Group)
		}

		if hasLeader && leaderSnapshot.SyncErr == nil {
			for _, slave := range leaderSnapshot.Sync.Slaves {
				if key := resolveSlaveMemberKey(slave, byKey, byHost); key != "" {
					memberKeys = append(memberKeys, key)
				}
			}
			memberKeys = uniqueStrings(memberKeys)
		}

		if grouped {
			if groupName == "" {
				if hasLeader && strings.TrimSpace(leaderSnapshot.Name) != "" {
					groupName = strings.TrimSpace(leaderSnapshot.Name)
				} else {
					groupName = leader
				}
			}

			groupRow := playerRow{
				Name:        groupName,
				IsGroup:     true,
				TellSlaves:  true,
				Grouping:    fmt.Sprintf("Grouped players: %d", len(memberKeys)),
				GroupDetail: "Overall volume",
				Kind:        rowKindGroup,
			}
			if hasLeader {
				groupRow.Device = leaderSnapshot.Device
				groupRow.Volume = leaderSnapshot.Volume
				groupRow.Mute = leaderSnapshot.Mute
				groupRow.PlaybackState = leaderSnapshot.PlaybackState
				groupRow.NowPlaying = leaderSnapshot.NowPlaying
				groupRow.Source = leaderSnapshot.Source
				groupRow.StatusDetail = leaderSnapshot.StatusText
				groupRow.Err = leaderSnapshot.Err
			} else {
				groupRow.Device = config.Device{ID: leader}
				groupRow.Err = fmt.Errorf("group master %s not discovered", leader)
			}

			// If leader doesn't expose track metadata, use the first playing member.
			if strings.TrimSpace(groupRow.NowPlaying) == "" {
				for _, memberKey := range memberKeys {
					member, ok := byKey[memberKey]
					if !ok {
						continue
					}
					if strings.TrimSpace(member.NowPlaying) != "" {
						groupRow.NowPlaying = member.NowPlaying
						if groupRow.PlaybackState == "" {
							groupRow.PlaybackState = member.PlaybackState
						}
						if strings.TrimSpace(groupRow.Source) == "" {
							groupRow.Source = member.Source
						}
						break
					}
				}
			}
			rows = append(rows, groupRow)

			for _, memberKey := range memberKeys {
				member, ok := byKey[memberKey]
				if !ok {
					memberName := sanitizeTerminalText(memberKey)
					if host := canonicalHostKey(memberKey); host != "" {
						memberName = host
					}
					memberGrouping := "Member"
					if groupName != "" {
						memberGrouping = "Member of " + groupName
					}
					rows = append(rows, playerRow{
						Device:        config.Device{ID: memberKey},
						Name:          memberName,
						Volume:        0,
						Mute:          false,
						PlaybackState: "",
						Indent:        3,
						TellSlaves:    false,
						Grouping:      memberGrouping,
						GroupDetail:   "Not discovered",
						StatusDetail:  "",
						Err:           fmt.Errorf("player not discovered"),
						Kind:          rowKindGroupMember,
					})
					continue
				}
				memberGrouping := "Member"
				if groupName != "" {
					memberGrouping = "Member of " + groupName
				}
				memberDetail := member.StatusText
				if memberDetail == "" {
					memberDetail = "Individual player volume"
				}
				rows = append(rows, playerRow{
					Device:        member.Device,
					Name:          member.Name,
					Volume:        member.Volume,
					Mute:          member.Mute,
					PlaybackState: member.PlaybackState,
					Indent:        3,
					TellSlaves:    false,
					NowPlaying:    "",
					Source:        "",
					Grouping:      memberGrouping,
					GroupDetail:   memberDetail,
					StatusDetail:  member.StatusText,
					Err:           member.Err,
					Kind:          rowKindGroupMember,
				})
			}
			continue
		}

		if !hasLeader {
			continue
		}
		grouping, groupDetail := summarizeGrouping(leaderSnapshot.Sync, leaderSnapshot.SyncErr)
		rows = append(rows, playerRow{
			Device:        leaderSnapshot.Device,
			Name:          leaderSnapshot.Name,
			Volume:        leaderSnapshot.Volume,
			Mute:          leaderSnapshot.Mute,
			PlaybackState: leaderSnapshot.PlaybackState,
			TellSlaves:    false,
			NowPlaying:    leaderSnapshot.NowPlaying,
			Source:        leaderSnapshot.Source,
			Grouping:      grouping,
			GroupDetail:   groupDetail,
			StatusDetail:  leaderSnapshot.StatusText,
			Err:           leaderSnapshot.Err,
			Kind:          rowKindStandalone,
		})
	}

	return rows
}

func findRowByKey(rows []playerRow, key string) (playerRow, int, bool) {
	if key == "" {
		return playerRow{}, -1, false
	}
	for i := range rows {
		if deviceKey(rows[i].Device) == key {
			return rows[i], i, true
		}
	}
	return playerRow{}, -1, false
}

func findMasterRowForMember(rows []playerRow, memberIndex int) (playerRow, bool) {
	if memberIndex <= 0 || memberIndex >= len(rows) {
		return playerRow{}, false
	}
	if !isGroupedMemberRow(rows[memberIndex]) {
		return playerRow{}, false
	}
	for i := memberIndex - 1; i >= 0; i-- {
		if rows[i].IsGroup {
			return rows[i], true
		}
		if !isGroupedMemberRow(rows[i]) {
			break
		}
	}
	return playerRow{}, false
}

func deviceHostPort(device config.Device) (string, int, error) {
	host := strings.TrimSpace(device.Host)
	port := device.Port
	if host != "" && port > 0 {
		return host, port, nil
	}
	return "", 0, fmt.Errorf("missing host/port on device %q", strings.TrimSpace(device.ID))
}

func canonicalHostPortKey(host string, port int) string {
	host = canonicalHostKey(host)
	if host == "" || port <= 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func canonicalHostKey(host string) string {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return ""
	}
	return host
}

func resolveSlaveMemberKey(slave bluos.SyncSlave, byKey map[string]playerSnapshot, byHost map[string][]string) string {
	rawID := strings.TrimSpace(slave.ID)
	if rawID == "" {
		return ""
	}
	if _, ok := byKey[rawID]; ok {
		return rawID
	}

	host := canonicalHostKey(rawID)
	if host == "" {
		return ""
	}

	if slave.Port > 0 {
		key := canonicalHostPortKey(host, slave.Port)
		if _, ok := byKey[key]; ok {
			return key
		}
	}

	candidates := uniqueStrings(byHost[host])
	if len(candidates) == 1 {
		return candidates[0]
	}
	if len(candidates) > 1 {
		sort.Strings(candidates)
		for _, candidate := range candidates {
			if strings.HasSuffix(candidate, ":11000") {
				return candidate
			}
		}
		return candidates[0]
	}

	if key := canonicalHostPortKey(host, 11000); key != "" {
		return key
	}
	return host
}

func leaderDisplayName(leader string, byKey map[string]playerSnapshot, membersByLeader map[string][]string, groupNameByLeader map[string]string) string {
	if group := sanitizeTerminalText(groupNameByLeader[leader]); group != "" {
		return group
	}
	if snapshot, ok := byKey[leader]; ok {
		if name := sanitizeTerminalText(snapshot.Name); name != "" {
			return name
		}
	}
	for _, memberKey := range membersByLeader[leader] {
		if snapshot, ok := byKey[memberKey]; ok {
			if name := sanitizeTerminalText(snapshot.Name); name != "" {
				return name
			}
		}
	}
	return sanitizeTerminalText(leader)
}

func leaderMemberName(memberKey string, byKey map[string]playerSnapshot) string {
	if snapshot, ok := byKey[memberKey]; ok {
		if name := sanitizeTerminalText(snapshot.Name); name != "" {
			return strings.ToLower(name)
		}
	}
	return strings.ToLower(sanitizeTerminalText(memberKey))
}

func uniqueStrings(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(input))
	for _, item := range input {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func summarizeGrouping(syncStatus bluos.SyncStatus, syncErr error) (string, string) {
	if syncErr != nil {
		return "Grouping unavailable", sanitizeTerminalText(syncErr.Error())
	}

	groupName := sanitizeTerminalText(syncStatus.Group)
	if syncStatus.Master != nil {
		host := sanitizeTerminalText(syncStatus.Master.Host)
		if host == "" {
			host = "master"
		}
		target := host
		if syncStatus.Master.Port > 0 {
			target = fmt.Sprintf("%s:%d", host, syncStatus.Master.Port)
		}
		if groupName != "" {
			return "Slave (" + groupName + ")", "Following " + target
		}
		return "Slave", "Following " + target
	}

	if len(syncStatus.Slaves) == 0 {
		if groupName != "" {
			return "Ungrouped", groupName
		}
		return "Ungrouped", "No linked players"
	}

	name := "Master"
	if groupName != "" {
		name = "Master (" + groupName + ")"
	}
	slaveIDs := make([]string, 0, len(syncStatus.Slaves))
	for _, slave := range syncStatus.Slaves {
		id := sanitizeTerminalText(slave.ID)
		if id != "" {
			slaveIDs = append(slaveIDs, id)
		}
	}
	if len(slaveIDs) == 0 {
		return name, fmt.Sprintf("%d linked players", len(syncStatus.Slaves)+1)
	}
	limit := 2
	shown := slaveIDs
	if len(slaveIDs) > limit {
		shown = slaveIDs[:limit]
	}
	detail := "Slaves: " + strings.Join(shown, ", ")
	if len(slaveIDs) > limit {
		detail += fmt.Sprintf(" +%d", len(slaveIDs)-limit)
	}
	return name, detail
}

func summarizeStatus(status bluos.Status) string {
	artist := strings.TrimSpace(status.Artist)
	title := strings.TrimSpace(status.Title)
	state := strings.TrimSpace(status.State)
	if artist == "" && title == "" {
		if state == "" {
			return ""
		}
		return strings.ToUpper(state)
	}
	if artist == "" {
		return title
	}
	if title == "" {
		return artist
	}
	return artist + " - " + title
}

func nowPlayingText(status bluos.Status) string {
	artist := strings.TrimSpace(status.Artist)
	title := strings.TrimSpace(status.Title)
	state := strings.ToLower(strings.TrimSpace(status.State))
	playing := isPlayingState(state)

	// Only show right-side playback text while actively playing/streaming.
	if !playing {
		return ""
	}
	if artist == "" && title == "" {
		return "PLAYING"
	}

	switch {
	case artist != "" && title != "":
		return artist + " - " + title
	case title != "":
		return title
	case artist != "":
		return artist
	default:
		return "PLAYING"
	}
}

func isPlayingState(state string) bool {
	if state == "" {
		return false
	}
	switch state {
	case "play", "playing", "stream", "streaming":
		return true
	default:
		return strings.Contains(state, "play") || strings.Contains(state, "stream")
	}
}

func normalizePlaybackState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func playbackToggleAction(state string) string {
	if isPlayingState(normalizePlaybackState(state)) {
		return "pause"
	}
	return "play"
}

func isGroupedMemberRow(row playerRow) bool {
	return row.Kind == rowKindGroupMember
}

func spacingLinesBetweenRows(current, next playerRow) []struct{} {
	if current.IsGroup && isGroupedMemberRow(next) {
		return nil
	}
	if isGroupedMemberRow(current) && isGroupedMemberRow(next) {
		return nil
	}
	if isGroupedMemberRow(current) {
		return make([]struct{}, 2)
	}
	return make([]struct{}, 2)
}

func groupedMemberNameColumnWidth(rows []playerRow, start, end, panelWidth int) int {
	maxWidth := 0
	for i := start; i < end && i < len(rows); i++ {
		row := rows[i]
		if !isGroupedMemberRow(row) {
			continue
		}
		w := textWidth("  " + strings.Repeat(" ", row.Indent) + row.Name)
		if w > maxWidth {
			maxWidth = w
		}
	}
	if maxWidth == 0 {
		maxWidth = 16
	}
	// Keep the volume close to names while reserving room for value text.
	maxCol := maxInt(12, panelWidth-10)
	if maxWidth+2 > maxCol {
		return maxCol
	}
	return maxWidth + 2
}

func formatCompactMemberLine(prefix, name, volume string, maxLen, nameCol int) string {
	if maxLen <= 0 {
		return ""
	}
	volume = strings.TrimSpace(volume)
	if volume == "" {
		return truncateTextPreserveLeft(prefix+name, maxLen)
	}

	availableName := maxLen - 1 - textWidth(volume)
	if availableName < 1 {
		return truncateText(volume, maxLen)
	}
	if nameCol > availableName {
		nameCol = availableName
	}
	if nameCol < 1 {
		nameCol = availableName
	}

	left := truncateTextPreserveLeft(prefix+name, nameCol)
	spaces := nameCol - textWidth(left)
	if spaces < 1 {
		spaces = 1
	}
	line := left + strings.Repeat(" ", spaces) + volume
	if textWidth(line) > maxLen {
		left = truncateTextPreserveLeft(prefix+name, maxLen-1-textWidth(volume))
		line = left + " " + volume
	}
	return line
}

func visibleWindow(total, selected, size int) (int, int) {
	if total <= 0 || size <= 0 {
		return 0, 0
	}
	if total <= size {
		return 0, total
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= total {
		selected = total - 1
	}
	start := selected - size/2
	if start < 0 {
		start = 0
	}
	end := start + size
	if end > total {
		end = total
		start = end - size
	}
	return start, end
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sanitizeTerminalText(s string) string {
	return strings.TrimSpace(stripTerminalControlChars(s))
}

func stripTerminalControlChars(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			b.WriteRune(' ')
		default:
			if unicode.IsControl(r) {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sanitizeTerminalError(err error) error {
	if err == nil {
		return nil
	}
	msg := sanitizeTerminalText(err.Error())
	if msg == "" {
		msg = "error"
	}
	return errors.New(msg)
}

func formatFooterBar(width int, leftText string, leftStyle lipgloss.Style, rightText string, rightStyle lipgloss.Style) string {
	if width <= 0 {
		return leftStyle.Render(sanitizeTerminalText(leftText))
	}
	leftText = sanitizeTerminalText(leftText)
	rightText = sanitizeTerminalText(rightText)
	if rightText == "" {
		return leftStyle.Render(truncateTextPreserveLeft(leftText, width))
	}

	maxRight := maxInt(8, width/2)
	if textWidth(rightText) > maxRight {
		rightText = truncateText(rightText, maxRight)
	}

	availableLeft := width - 1 - textWidth(rightText)
	if availableLeft < 1 {
		return rightStyle.Render(truncateText(rightText, width))
	}
	leftText = truncateTextPreserveLeft(leftText, availableLeft)

	spaces := width - textWidth(leftText) - textWidth(rightText)
	if spaces < 1 {
		spaces = 1
	}
	return leftStyle.Render(leftText) + strings.Repeat(" ", spaces) + rightStyle.Render(rightText)
}

func truncateText(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if maxLen <= 0 {
		return ""
	}
	if textWidth(s) <= maxLen {
		return s
	}
	if maxLen == 1 {
		return truncateToWidth(s, 1)
	}
	return truncateToWidth(s, maxLen-1) + "~"
}

func truncateTextPreserveLeft(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if textWidth(s) <= maxLen {
		return s
	}
	if maxLen == 1 {
		return truncateToWidth(s, 1)
	}
	return truncateToWidth(s, maxLen-1) + "~"
}

func formatHeadlineLine(prefix, headline, right string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	headline = sanitizeTerminalText(headline)
	right = sanitizeTerminalText(right)
	left := prefix + headline
	if right == "" {
		return truncateTextPreserveLeft(left, maxLen)
	}

	// Keep the right side readable and right-aligned.
	maxRight := maxInt(8, maxLen/2)
	if textWidth(right) > maxRight {
		right = truncateText(right, maxRight)
	}

	availableLeft := maxLen - 1 - textWidth(right)
	if availableLeft < 1 {
		return truncateText(right, maxLen)
	}
	left = truncateTextPreserveLeft(left, availableLeft)

	spaces := maxLen - textWidth(left) - textWidth(right)
	if spaces < 1 {
		spaces = 1
	}
	return left + strings.Repeat(" ", spaces) + right
}

func truncateToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 || s == "" {
		return ""
	}
	var b strings.Builder
	width := 0
	for _, r := range s {
		rw := textWidth(string(r))
		if width+rw > maxWidth {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	return b.String()
}

func textWidth(s string) int {
	return lipgloss.Width(s)
}

func volumeBar(level int, width int) string {
	if width <= 0 {
		return ""
	}
	level = clampInt(level, 0, 100)
	filled := (level*width + 50) / 100
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("━", filled) + strings.Repeat("╌", width-filled)
}
