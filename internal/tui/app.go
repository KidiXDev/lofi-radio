package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kidixdev/lofi-radio/internal/bootstrap"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"golang.org/x/term"
)

type viewMode int

const (
	viewBoot viewMode = iota
	viewSelect
	viewResolving
	viewPlayer
	viewError
)

type asyncKind string

const (
	asyncBootstrap asyncKind = "bootstrap"
	asyncResolve   asyncKind = "resolve"
)

type asyncResult struct {
	kind      asyncKind
	stations  []radio.Station
	station   radio.Station
	streamURL string
	err       error
}

type inputEvent struct {
	name string
	ch   rune
}

type App struct {
	playlistURL string

	mode        viewMode
	status      string
	errMessage  string
	footerHint  string
	stations    []radio.Station
	selectedIdx int

	bootEvent bootstrap.ProgressEvent

	player          *radio.Player
	currentStation  radio.Station
	volume          int
	playing         bool
	paused          bool
	playStartedAt   time.Time
	pausedStartedAt time.Time
	totalPaused     time.Duration

	terminal *terminalSession
	inputCh  chan inputEvent
	resultCh chan asyncResult
	bootCh   chan bootstrap.ProgressEvent
	doneCh   chan struct{}
}

func Run(playlistURL string) error {
	terminalUI, err := newTerminalSession(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer terminalUI.Close()

	app := &App{
		playlistURL: playlistURL,
		mode:        viewBoot,
		status:      "Checking dependencies",
		footerHint:  "Press q to quit",
		volume:      55,
		player:      radio.NewPlayer(55),
		terminal:    terminalUI,
		inputCh:     make(chan inputEvent, 32),
		resultCh:    make(chan asyncResult, 8),
		bootCh:      make(chan bootstrap.ProgressEvent, 32),
		doneCh:      make(chan struct{}),
	}

	go app.captureInput()
	app.startBootstrap()
	return app.loop()
}

func (a *App) loop() error {
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	defer close(a.doneCh)
	defer a.player.Stop()

	for {
		a.terminal.Render(a.render())

		select {
		case <-ticker.C:
			if a.mode == viewPlayer && a.playing && !a.player.IsRunning() {
				a.playing = false
				a.mode = viewError
				a.errMessage = "playback stopped"
				a.footerHint = "Press q to return"
			}
		case key := <-a.inputCh:
			if a.handleKey(key) {
				if a.errMessage != "" {
					return errors.New(a.errMessage)
				}
				return nil
			}
		case event := <-a.bootCh:
			a.bootEvent = event
			if event.Type == bootstrap.ProgressEventStatus && strings.TrimSpace(event.Message) != "" {
				a.status = event.Message
			}
		case result := <-a.resultCh:
			if err := a.handleResult(result); err != nil {
				return err
			}
		}
	}
}

func (a *App) startBootstrap() {
	go func() {
		reporter := bootstrap.ProgressReporterFunc(func(event bootstrap.ProgressEvent) {
			select {
			case a.bootCh <- event:
			default:
			}
		})

		if _, err := bootstrap.EnsureDependenciesWithProgress(reporter); err != nil {
			a.resultCh <- asyncResult{kind: asyncBootstrap, err: err}
			return
		}

		stations, err := radio.FetchStationsFromPlaylist(a.playlistURL)
		a.resultCh <- asyncResult{
			kind:     asyncBootstrap,
			stations: stations,
			err:      err,
		}
	}()
}

func (a *App) startResolve(station radio.Station) {
	a.mode = viewResolving
	a.status = "Resolving direct stream URL"
	a.footerHint = "Press q to cancel"

	go func(st radio.Station) {
		streamURL, err := radio.GetDirectAudioURL(st.VideoURL)
		a.resultCh <- asyncResult{
			kind:      asyncResolve,
			station:   st,
			streamURL: streamURL,
			err:       err,
		}
	}(station)
}

func (a *App) handleResult(result asyncResult) error {
	switch result.kind {
	case asyncBootstrap:
		if result.err != nil {
			a.mode = viewError
			a.errMessage = fmt.Sprintf("bootstrap failed: %v", result.err)
			a.footerHint = "Press q to exit"
			return nil
		}

		if len(result.stations) == 0 {
			a.mode = viewError
			a.errMessage = "no available stations found"
			a.footerHint = "Press q to exit"
			return nil
		}

		a.stations = result.stations
		a.selectedIdx = 0
		a.mode = viewSelect
		a.status = "Select a station"
		a.footerHint = "Enter play  Up/Down navigate  q quit"

	case asyncResolve:
		if result.err != nil {
			a.mode = viewError
			a.errMessage = fmt.Sprintf("stream resolution failed: %v", result.err)
			a.footerHint = "Press q to go back"
			return nil
		}

		if err := a.player.Play(result.streamURL); err != nil {
			a.mode = viewError
			a.errMessage = fmt.Sprintf("playback failed: %v", err)
			a.footerHint = "Press q to go back"
			return nil
		}

		a.currentStation = result.station
		a.playing = true
		a.paused = false
		a.playStartedAt = time.Now()
		a.totalPaused = 0
		a.pausedStartedAt = time.Time{}
		a.mode = viewPlayer
		a.status = "Playing"
		a.footerHint = "Space pause/resume  +/- volume  s stations  q quit"
	}

	return nil
}

func (a *App) handleKey(event inputEvent) bool {
	if event.name == "ctrl_c" {
		return true
	}

	switch a.mode {
	case viewBoot:
		if event.ch == 'q' || event.name == "escape" {
			return true
		}
	case viewSelect:
		return a.handleSelectKey(event)
	case viewResolving:
		if event.ch == 'q' || event.name == "escape" {
			return true
		}
	case viewPlayer:
		return a.handlePlayerKey(event)
	case viewError:
		if event.ch == 'q' || event.name == "enter" || event.name == "escape" {
			if len(a.stations) > 0 {
				a.mode = viewSelect
				a.errMessage = ""
				a.status = "Select a station"
				a.footerHint = "Enter play  Up/Down navigate  q quit"
				return false
			}
			return true
		}
	}

	return false
}

func (a *App) handleSelectKey(event inputEvent) bool {
	switch {
	case event.name == "up" || event.ch == 'k' || event.ch == 'w':
		if a.selectedIdx > 0 {
			a.selectedIdx--
		}
	case event.name == "down" || event.ch == 'j':
		if a.selectedIdx < len(a.stations)-1 {
			a.selectedIdx++
		}
	case event.name == "enter":
		if len(a.stations) == 0 {
			return false
		}
		a.startResolve(a.stations[a.selectedIdx])
	case event.ch == 'q' || event.name == "escape":
		return true
	}

	return false
}

func (a *App) handlePlayerKey(event inputEvent) bool {
	switch {
	case event.name == "space" || event.ch == 'p':
		if err := a.player.PauseToggle(); err == nil {
			if a.paused {
				a.paused = false
				a.totalPaused += time.Since(a.pausedStartedAt)
				a.pausedStartedAt = time.Time{}
				a.status = "Playing"
			} else {
				a.paused = true
				a.pausedStartedAt = time.Now()
				a.status = "Paused"
			}
		}

	case event.ch == '+' || event.ch == '=' || event.ch == '0':
		vol, err := a.player.IncreaseVolume(5)
		if err == nil {
			a.volume = vol
		}

	case event.ch == '-' || event.ch == '_' || event.ch == '9':
		vol, err := a.player.DecreaseVolume(5)
		if err == nil {
			a.volume = vol
		}

	case event.ch == 's':
		a.mode = viewSelect
		a.status = "Pick another station"
		a.footerHint = "Enter play  Up/Down navigate  q quit"

	case event.ch == 'q' || event.name == "escape":
		return true
	}

	return false
}

func (a *App) render() string {
	switch a.mode {
	case viewBoot:
		return a.renderBoot()
	case viewSelect:
		return a.renderSelector()
	case viewResolving:
		return a.renderResolving()
	case viewPlayer:
		return a.renderPlayer()
	case viewError:
		return a.renderError()
	default:
		return "Unknown view\n"
	}
}

func (a *App) renderBoot() string {
	var b strings.Builder
	b.WriteString(colorText(" LOFI RADIO ", styleAccent))
	b.WriteString("\n")
	b.WriteString(colorText("Bootstrapping", styleMuted))
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("Status: %s\n", a.status))
	if a.bootEvent.Type == bootstrap.ProgressEventDownload {
		label := a.bootEvent.Component
		if label == "" {
			label = "download"
		}
		bar := renderBar(a.bootEvent.Download.BytesReceived, a.bootEvent.Download.TotalBytes, 34)
		speed := humanSpeed(a.bootEvent.Download.SpeedPerSec)
		eta := humanDuration(a.bootEvent.Download.ETA)
		b.WriteString(fmt.Sprintf("%s %s\n", strings.ToUpper(label), bar))
		totalLabel := "unknown"
		if a.bootEvent.Download.TotalBytes > 0 {
			totalLabel = humanBytes(a.bootEvent.Download.TotalBytes)
		}
		b.WriteString(fmt.Sprintf("Data  %s / %s\n", humanBytes(a.bootEvent.Download.BytesReceived), totalLabel))
		b.WriteString(fmt.Sprintf("Speed %s | ETA %s\n", speed, eta))
	} else {
		b.WriteString("Waiting for progress data...\n")
	}

	b.WriteString("\n")
	b.WriteString(colorText(a.footerHint, styleMuted))
	b.WriteString("\n")
	return b.String()
}

func (a *App) renderSelector() string {
	var b strings.Builder
	b.WriteString(colorText(" LOFI RADIO ", styleAccent))
	b.WriteString("\n")
	b.WriteString(colorText("Station Selection", styleMuted))
	b.WriteString("\n\n")

	start := 0
	maxRows := 12
	if a.selectedIdx >= maxRows {
		start = a.selectedIdx - maxRows + 1
	}
	end := start + maxRows
	if end > len(a.stations) {
		end = len(a.stations)
	}

	for i := start; i < end; i++ {
		prefix := "  "
		if i == a.selectedIdx {
			prefix = colorText("▶ ", styleAccent)
		}

		title := compactText(a.stations[i].Title, 68)
		if i == a.selectedIdx {
			b.WriteString(colorText(prefix+title, styleHighlight))
		} else {
			b.WriteString(prefix + title)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("Station %d/%d\n", a.selectedIdx+1, len(a.stations)))
	b.WriteString(colorText(a.footerHint, styleMuted))
	b.WriteString("\n")
	return b.String()
}

func (a *App) renderResolving() string {
	var b strings.Builder
	b.WriteString(colorText(" LOFI RADIO ", styleAccent))
	b.WriteString("\n")
	b.WriteString(colorText("Preparing Stream", styleMuted))
	b.WriteString("\n\n")
	b.WriteString("Resolving direct audio URL from source...\n")
	b.WriteString("This can take a few seconds depending on network conditions.\n")
	b.WriteString("\n")
	b.WriteString(colorText(a.footerHint, styleMuted))
	b.WriteString("\n")
	return b.String()
}

func (a *App) renderPlayer() string {
	var b strings.Builder
	b.WriteString(colorText(" LOFI RADIO PLAYER ", styleAccent))
	b.WriteString("\n")
	b.WriteString(colorText("Now Playing", styleMuted))
	b.WriteString("\n\n")

	title := compactText(a.currentStation.Title, 72)
	b.WriteString(fmt.Sprintf("Title   %s\n", title))
	b.WriteString(fmt.Sprintf("Status  %s\n", a.status))

	volBar := renderBar(int64(a.volume), 100, 24)
	b.WriteString(fmt.Sprintf("Volume  %s %d%%\n", volBar, a.volume))

	elapsed := a.playbackElapsed()
	b.WriteString(fmt.Sprintf("Time    %s\n", elapsed))
	b.WriteString("\n")
	b.WriteString(colorText(a.footerHint, styleMuted))
	b.WriteString("\n")
	return b.String()
}

func (a *App) renderError() string {
	var b strings.Builder
	b.WriteString(colorText(" LOFI RADIO ", styleAccent))
	b.WriteString("\n")
	b.WriteString(colorText("Error", styleError))
	b.WriteString("\n\n")
	b.WriteString(a.errMessage)
	b.WriteString("\n\n")
	b.WriteString(colorText(a.footerHint, styleMuted))
	b.WriteString("\n")
	return b.String()
}

func (a *App) playbackElapsed() string {
	if a.playStartedAt.IsZero() {
		return "00:00"
	}

	elapsed := time.Since(a.playStartedAt) - a.totalPaused
	if a.paused {
		elapsed = a.pausedStartedAt.Sub(a.playStartedAt) - a.totalPaused
	}
	if elapsed < 0 {
		elapsed = 0
	}

	hours := int(elapsed.Hours())
	minutes := int(elapsed.Minutes()) % 60
	seconds := int(elapsed.Seconds()) % 60
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func (a *App) captureInput() {
	reader := os.Stdin
	parser := &keyParser{}
	buffer := make([]byte, 64)

	for {
		select {
		case <-a.doneCh:
			return
		default:
		}

		readCount, err := reader.Read(buffer)
		if err != nil {
			if err == io.EOF {
				return
			}
			continue
		}
		if readCount == 0 {
			continue
		}

		events := parser.feed(buffer[:readCount])
		for _, ev := range events {
			select {
			case a.inputCh <- ev:
			default:
			}
		}
	}
}

type keyParser struct {
	pending []byte
}

func (p *keyParser) feed(data []byte) []inputEvent {
	p.pending = append(p.pending, data...)
	events := make([]inputEvent, 0, len(data))

	for len(p.pending) > 0 {
		current := p.pending[0]

		if current == 0x1b {
			if len(p.pending) < 2 {
				break
			}
			if p.pending[1] == '[' {
				if len(p.pending) < 3 {
					break
				}
				switch p.pending[2] {
				case 'A':
					events = append(events, inputEvent{name: "up"})
				case 'B':
					events = append(events, inputEvent{name: "down"})
				case 'C':
					events = append(events, inputEvent{name: "right"})
				case 'D':
					events = append(events, inputEvent{name: "left"})
				default:
					events = append(events, inputEvent{name: "escape"})
				}
				p.pending = p.pending[3:]
				continue
			}

			events = append(events, inputEvent{name: "escape"})
			p.pending = p.pending[1:]
			continue
		}

		p.pending = p.pending[1:]
		switch current {
		case 3:
			events = append(events, inputEvent{name: "ctrl_c"})
		case '\r', '\n':
			events = append(events, inputEvent{name: "enter"})
		case ' ':
			events = append(events, inputEvent{name: "space"})
		default:
			if current >= 32 && current <= 126 {
				events = append(events, inputEvent{ch: rune(current)})
			}
		}
	}

	return events
}

type terminalSession struct {
	in    *os.File
	out   *os.File
	state *term.State
}

func newTerminalSession(in, out *os.File) (*terminalSession, error) {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return nil, fmt.Errorf("interactive terminal required")
	}

	rawState, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}

	session := &terminalSession{
		in:    in,
		out:   out,
		state: rawState,
	}

	_, _ = fmt.Fprint(out, "\x1b[?1049h\x1b[2J\x1b[H\x1b[?25l")
	return session, nil
}

func (t *terminalSession) Close() {
	if t == nil {
		return
	}

	if t.state != nil {
		_ = term.Restore(int(t.in.Fd()), t.state)
	}

	_, _ = fmt.Fprint(t.out, "\x1b[?25h\x1b[?1049l")
}

func (t *terminalSession) Render(content string) {
	var out bytes.Buffer
	out.WriteString("\x1b[H\x1b[2J")
	out.WriteString(content)
	_, _ = io.Copy(t.out, &out)
}

const (
	styleAccent    = "36"
	styleMuted     = "90"
	styleHighlight = "97"
	styleError     = "31"
)

func colorText(value, colorCode string) string {
	return "\x1b[" + colorCode + "m" + value + "\x1b[0m"
}

func renderBar(current, total int64, width int) string {
	if width < 4 {
		width = 4
	}

	if total <= 0 {
		total = current
		if total <= 0 {
			total = 1
		}
	}

	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}

	filled := int((float64(current) / float64(total)) * float64(width))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}

	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func humanBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}

	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(size)
	unitIndex := -1
	for value >= 1024 && unitIndex < len(units)-1 {
		value /= 1024
		unitIndex++
	}

	if unitIndex < 0 {
		return fmt.Sprintf("%d B", size)
	}
	return fmt.Sprintf("%.1f %s", value, units[unitIndex])
}

func humanSpeed(speedBytes float64) string {
	if speedBytes <= 0 {
		return "0 B/s"
	}
	return humanBytes(int64(speedBytes)) + "/s"
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "--:--"
	}
	totalSeconds := int(d.Seconds())
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60
	hours := minutes / 60
	minutes = minutes % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func compactText(value string, maxLen int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if maxLen <= 0 || len(clean) <= maxLen {
		return clean
	}

	if maxLen <= 3 {
		return clean[:maxLen]
	}

	return clean[:maxLen-3] + "..."
}
