package tui

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	gotui "github.com/grindlemire/go-tui"
	"github.com/kidixdev/lofi-radio/internal/bootstrap"
	"github.com/kidixdev/lofi-radio/internal/radio"
)

type viewMode int

const (
	viewBoot viewMode = iota
	viewSelect
	viewResolving
	viewPlayer
	viewError
)

const (
	selectHint = "Enter play  Up/Down or k/j navigate  q quit"
	playerHint = "Space pause/resume  +/- volume  s stations  q quit"
)

type asyncKind string

const (
	asyncBootstrap asyncKind = "bootstrap"
	asyncResolve   asyncKind = "resolve"
)

type asyncResult struct {
	kind         asyncKind
	stations     []radio.Station
	station      radio.Station
	streamURL    string
	resolveToken int
	err          error
}

type app struct {
	playlistURL string

	mode       *gotui.State[viewMode]
	status     *gotui.State[string]
	errMessage *gotui.State[string]
	footerHint *gotui.State[string]
	stations   *gotui.State[[]radio.Station]
	selected   *gotui.State[int]
	bootEvent  *gotui.State[bootstrap.ProgressEvent]

	player          *radio.Player
	currentStation  *gotui.State[radio.Station]
	volume          *gotui.State[int]
	playing         *gotui.State[bool]
	paused          *gotui.State[bool]
	playStartedAt   *gotui.State[time.Time]
	pausedStartedAt *gotui.State[time.Time]
	totalPaused     *gotui.State[time.Duration]
	now             *gotui.State[time.Time]

	// animation
	spinnerFrame *gotui.State[int]
	wavePhase    *gotui.State[float64]
	pulsePhase   *gotui.State[float64]
	aniTick      int

	resolveToken *gotui.State[int]
	bootCh       chan bootstrap.ProgressEvent
	resultCh     chan asyncResult

	exitErr error
	fatal   bool
}

func Run(playlistURL string) error {
	component := newApp(playlistURL)

	ui, err := gotui.NewApp(
		gotui.WithRootComponent(component),
	)
	if err != nil {
		return err
	}
	defer ui.Close()
	defer component.player.Stop()

	if err := ui.Run(); err != nil {
		return err
	}

	return component.exitErr
}

func newApp(playlistURL string) *app {
	component := &app{
		playlistURL: playlistURL,

		mode:       gotui.NewState(viewBoot),
		status:     gotui.NewState("Checking dependencies"),
		errMessage: gotui.NewState(""),
		footerHint: gotui.NewState("Press q to quit"),
		stations:   gotui.NewState([]radio.Station{}),
		selected:   gotui.NewState(0),
		bootEvent:  gotui.NewState(bootstrap.ProgressEvent{}),

		player:          radio.NewPlayer(55),
		currentStation:  gotui.NewState(radio.Station{}),
		volume:          gotui.NewState(55),
		playing:         gotui.NewState(false),
		paused:          gotui.NewState(false),
		playStartedAt:   gotui.NewState(time.Time{}),
		pausedStartedAt: gotui.NewState(time.Time{}),
		totalPaused:     gotui.NewState(time.Duration(0)),
		now:             gotui.NewState(time.Now()),

		spinnerFrame: gotui.NewState(0),
		wavePhase:    gotui.NewState(0.0),
		pulsePhase:   gotui.NewState(0.0),

		resolveToken: gotui.NewState(0),
		bootCh:       make(chan bootstrap.ProgressEvent, 64),
		resultCh:     make(chan asyncResult, 8),
	}

	component.startBootstrap()
	return component
}

func (a *app) BindApp(ui *gotui.App) {
	a.mode.BindApp(ui)
	a.status.BindApp(ui)
	a.errMessage.BindApp(ui)
	a.footerHint.BindApp(ui)
	a.stations.BindApp(ui)
	a.selected.BindApp(ui)
	a.bootEvent.BindApp(ui)
	a.currentStation.BindApp(ui)
	a.volume.BindApp(ui)
	a.playing.BindApp(ui)
	a.paused.BindApp(ui)
	a.playStartedAt.BindApp(ui)
	a.pausedStartedAt.BindApp(ui)
	a.totalPaused.BindApp(ui)
	a.now.BindApp(ui)
	a.resolveToken.BindApp(ui)
	a.spinnerFrame.BindApp(ui)
	a.wavePhase.BindApp(ui)
	a.pulsePhase.BindApp(ui)
}

func (a *app) Watchers() []gotui.Watcher {
	return []gotui.Watcher{
		gotui.Watch(a.bootCh, a.onBootProgress),
		gotui.Watch(a.resultCh, a.onAsyncResult),
		gotui.OnTimer(80*time.Millisecond, a.onTick),
	}
}

func (a *app) KeyMap() gotui.KeyMap {
	return gotui.KeyMap{
		gotui.OnStop(gotui.KeyCtrlC, func(ke gotui.KeyEvent) {
			ke.App().Stop()
		}),
		gotui.OnStop(gotui.KeyEscape, func(ke gotui.KeyEvent) {
			a.handleQuitOrBack(ke)
		}),
		gotui.OnStop(gotui.Rune('q'), func(ke gotui.KeyEvent) {
			a.handleQuitOrBack(ke)
		}),
		gotui.OnStop(gotui.KeyUp, func(gotui.KeyEvent) {
			a.moveSelection(-1)
		}),
		gotui.OnStop(gotui.Rune('k'), func(gotui.KeyEvent) {
			a.moveSelection(-1)
		}),
		gotui.OnStop(gotui.Rune('w'), func(gotui.KeyEvent) {
			a.moveSelection(-1)
		}),
		gotui.OnStop(gotui.KeyDown, func(gotui.KeyEvent) {
			a.moveSelection(1)
		}),
		gotui.OnStop(gotui.Rune('j'), func(gotui.KeyEvent) {
			a.moveSelection(1)
		}),
		gotui.OnStop(gotui.KeyEnter, func(ke gotui.KeyEvent) {
			a.handleEnter(ke)
		}),
		gotui.OnStop(gotui.Rune(' '), func(gotui.KeyEvent) {
			a.togglePause()
		}),
		gotui.OnStop(gotui.Rune('p'), func(gotui.KeyEvent) {
			a.togglePause()
		}),
		gotui.OnStop(gotui.Rune('+'), func(gotui.KeyEvent) {
			a.adjustVolume(5)
		}),
		gotui.OnStop(gotui.Rune('='), func(gotui.KeyEvent) {
			a.adjustVolume(5)
		}),
		gotui.OnStop(gotui.Rune('0'), func(gotui.KeyEvent) {
			a.adjustVolume(5)
		}),
		gotui.OnStop(gotui.Rune('-'), func(gotui.KeyEvent) {
			a.adjustVolume(-5)
		}),
		gotui.OnStop(gotui.Rune('_'), func(gotui.KeyEvent) {
			a.adjustVolume(-5)
		}),
		gotui.OnStop(gotui.Rune('9'), func(gotui.KeyEvent) {
			a.adjustVolume(-5)
		}),
		gotui.OnStop(gotui.Rune('s'), func(gotui.KeyEvent) {
			if a.mode.Get() == viewPlayer {
				a.goToSelector("Pick another station")
			}
		}),
	}
}

func (a *app) onBootProgress(event bootstrap.ProgressEvent) {
	a.bootEvent.Set(event)
	if event.Type == bootstrap.ProgressEventStatus && strings.TrimSpace(event.Message) != "" {
		a.status.Set(event.Message)
	}
}

func (a *app) onAsyncResult(result asyncResult) {
	switch result.kind {
	case asyncBootstrap:
		if result.err != nil {
			a.setFatalError(
				fmt.Errorf("bootstrap failed: %w", result.err),
				fmt.Sprintf("bootstrap failed: %v", result.err),
			)
			return
		}

		if len(result.stations) == 0 {
			a.setFatalError(errors.New("no available stations found"), "no available stations found")
			return
		}

		a.stations.Set(result.stations)
		a.selected.Set(0)
		a.mode.Set(viewSelect)
		a.status.Set("Select a station")
		a.footerHint.Set(selectHint)
		a.errMessage.Set("")
		a.fatal = false
		a.exitErr = nil

	case asyncResolve:
		if result.resolveToken != a.resolveToken.Get() {
			return
		}

		if result.err != nil {
			a.setTransientError(fmt.Sprintf("stream resolution failed: %v", result.err))
			return
		}

		if err := a.player.Play(result.streamURL); err != nil {
			a.setTransientError(fmt.Sprintf("playback failed: %v", err))
			return
		}

		now := time.Now()
		a.currentStation.Set(result.station)
		a.playing.Set(true)
		a.paused.Set(false)
		a.playStartedAt.Set(now)
		a.pausedStartedAt.Set(time.Time{})
		a.totalPaused.Set(0)
		a.now.Set(now)
		a.volume.Set(a.player.Volume())
		a.mode.Set(viewPlayer)
		a.status.Set("Playing")
		a.footerHint.Set(playerHint)
	}
}

func (a *app) onTick() {
	a.aniTick++

	// Advance spinner every 5 ticks (~400ms)
	if a.aniTick%5 == 0 {
		a.spinnerFrame.Update(func(v int) int { return v + 1 })
	}
	// Wave and pulse advance every tick
	a.wavePhase.Update(func(v float64) float64 { return v + 0.07 })
	a.pulsePhase.Update(func(v float64) float64 { return v + 0.04 })

	if a.mode.Get() != viewPlayer || !a.playing.Get() {
		return
	}

	if !a.player.IsRunning() {
		a.playing.Set(false)
		a.paused.Set(false)
		a.setTransientError("playback stopped")
		return
	}

	if !a.paused.Get() {
		a.now.Set(time.Now())
	}
}

func (a *app) handleQuitOrBack(ke gotui.KeyEvent) {
	switch a.mode.Get() {
	case viewResolving:
		a.resolveToken.Update(func(v int) int { return v + 1 })
		a.goToSelector("Select a station")
		return

	case viewError:
		if len(a.stations.Get()) > 0 && !a.fatal {
			a.goToSelector("Select a station")
			return
		}

		if a.exitErr == nil {
			msg := strings.TrimSpace(a.errMessage.Get())
			if msg != "" {
				a.exitErr = errors.New(msg)
			}
		}
		ke.App().Stop()
		return
	}

	ke.App().Stop()
}

func (a *app) handleEnter(ke gotui.KeyEvent) {
	switch a.mode.Get() {
	case viewSelect:
		stations := a.stations.Get()
		if len(stations) == 0 {
			return
		}

		selected := clamp(a.selected.Get(), 0, len(stations)-1)
		a.selected.Set(selected)
		a.startResolve(stations[selected])

	case viewError:
		a.handleQuitOrBack(ke)
	}
}

func (a *app) moveSelection(delta int) {
	if a.mode.Get() != viewSelect {
		return
	}

	stations := a.stations.Get()
	if len(stations) == 0 || delta == 0 {
		return
	}

	a.selected.Set(clamp(a.selected.Get()+delta, 0, len(stations)-1))
}

func (a *app) togglePause() {
	if a.mode.Get() != viewPlayer {
		return
	}

	if err := a.player.PauseToggle(); err != nil {
		return
	}

	if a.paused.Get() {
		a.paused.Set(false)

		started := a.pausedStartedAt.Get()
		if !started.IsZero() {
			a.totalPaused.Set(a.totalPaused.Get() + time.Since(started))
		}

		a.pausedStartedAt.Set(time.Time{})
		a.status.Set("Playing")
		a.now.Set(time.Now())
		return
	}

	a.paused.Set(true)
	a.pausedStartedAt.Set(time.Now())
	a.status.Set("Paused")
}

func (a *app) adjustVolume(delta int) {
	if a.mode.Get() != viewPlayer {
		return
	}

	var (
		volume int
		err    error
	)

	if delta > 0 {
		volume, err = a.player.IncreaseVolume(delta)
	} else {
		volume, err = a.player.DecreaseVolume(-delta)
	}
	if err != nil {
		return
	}

	a.volume.Set(volume)
}

func (a *app) goToSelector(status string) {
	if a.mode.Get() != viewPlayer && a.mode.Get() != viewSelect && a.mode.Get() != viewResolving && a.mode.Get() != viewError {
		return
	}

	if len(a.stations.Get()) == 0 {
		return
	}

	a.mode.Set(viewSelect)
	a.status.Set(status)
	a.footerHint.Set(selectHint)
	a.errMessage.Set("")
	if a.fatal {
		a.fatal = false
		a.exitErr = nil
	}
}

func (a *app) setTransientError(message string) {
	a.mode.Set(viewError)
	a.errMessage.Set(message)
	a.footerHint.Set("Press q or Enter to return")
	a.fatal = false
	a.exitErr = nil
}

func (a *app) setFatalError(err error, message string) {
	a.mode.Set(viewError)
	a.errMessage.Set(message)
	a.footerHint.Set("Press q or Enter to exit")
	a.fatal = true

	if err != nil {
		a.exitErr = err
		return
	}
	a.exitErr = errors.New(message)
}

func (a *app) startBootstrap() {
	go func() {
		reporter := bootstrap.ProgressReporterFunc(func(event bootstrap.ProgressEvent) {
			select {
			case a.bootCh <- event:
			default:
			}
		})

		if _, err := bootstrap.EnsureDependenciesWithProgress(reporter); err != nil {
			a.emitResult(asyncResult{kind: asyncBootstrap, err: err})
			return
		}

		stations, err := radio.FetchStationsFromPlaylist(a.playlistURL)
		a.emitResult(asyncResult{
			kind:     asyncBootstrap,
			stations: stations,
			err:      err,
		})
	}()
}

func (a *app) startResolve(station radio.Station) {
	token := a.resolveToken.Get() + 1
	a.resolveToken.Set(token)
	a.mode.Set(viewResolving)
	a.status.Set("Resolving direct stream URL")
	a.footerHint.Set("Press q to cancel")
	a.errMessage.Set("")

	go func(resolveToken int, st radio.Station) {
		streamURL, err := radio.GetDirectAudioURL(st.VideoURL)
		a.emitResult(asyncResult{
			kind:         asyncResolve,
			station:      st,
			streamURL:    streamURL,
			resolveToken: resolveToken,
			err:          err,
		})
	}(token, station)
}

func (a *app) emitResult(result asyncResult) {
	select {
	case a.resultCh <- result:
	default:
	}
}

func (a *app) Render(ui *gotui.App) *gotui.Element {
	_ = ui

	// Pulsing border color: oscillates between cyan and purple
	pulse := a.pulsePhase.Get()
	t := (math.Sin(pulse) + 1) / 2
	purple, _ := gotui.HexColor("#9B59B6")
	borderColor := gotui.NewGradient(gotui.Cyan, purple).At(t)

	root := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithHeightPercent(100),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(borderColor)),
		gotui.WithPaddingTRBL(1, 2, 1, 2),
		gotui.WithGap(1),
	)

	// Header row: title + spinner
	header := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(2),
	)
	header.AddChild(gotui.New(
		gotui.WithText("◈ LOFI RADIO"),
		gotui.WithTextGradient(gotui.NewGradient(gotui.Cyan, gotui.Magenta).WithDirection(gotui.GradientHorizontal)),
		gotui.WithTextStyle(gotui.NewStyle().Bold()),
	))
	header.AddChild(gotui.New(
		gotui.WithText(a.modeLabel()),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim().Italic()),
	))
	root.AddChild(header)
	root.AddChild(gotui.New(gotui.WithHR()))

	content := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
	)
	switch a.mode.Get() {
	case viewBoot:
		content.AddChild(a.renderBoot())
	case viewSelect:
		content.AddChild(a.renderSelector())
	case viewResolving:
		content.AddChild(a.renderResolving())
	case viewPlayer:
		content.AddChild(a.renderPlayer())
	case viewError:
		content.AddChild(a.renderError())
	}
	root.AddChild(content)

	root.AddChild(gotui.New(gotui.WithHR()))
	root.AddChild(gotui.New(
		gotui.WithText(a.footerHint.Get()),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))

	return root
}

var spinnerBraille = []string{"⠋", "⠙", "⠸", "⠴", "⠦", "⠇"}

func (a *app) renderBoot() *gotui.Element {
	box := contentBox()

	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]
	statusRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(1),
	)
	statusRow.AddChild(gotui.New(
		gotui.WithText(spin),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.Cyan)),
	))
	statusRow.AddChild(gotui.New(
		gotui.WithText(a.status.Get()),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite)),
	))
	box.AddChild(statusRow)

	event := a.bootEvent.Get()
	if event.Type == bootstrap.ProgressEventDownload {
		label := strings.ToUpper(strings.TrimSpace(event.Component))
		if label == "" {
			label = "DOWNLOAD"
		}
		totalLabel := "unknown"
		if event.Download.TotalBytes > 0 {
			totalLabel = humanBytes(event.Download.TotalBytes)
		}
		box.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("%-10s %s", label, renderFancyBar(event.Download.BytesReceived, event.Download.TotalBytes, 32))),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan)),
		))
		box.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("  Data   %s / %s", humanBytes(event.Download.BytesReceived), totalLabel)),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
		box.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("  Speed  %s  ETA %s", humanSpeed(event.Download.SpeedPerSec), humanDuration(event.Download.ETA))),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
	}

	return box
}

func (a *app) renderSelector() *gotui.Element {
	box := contentBox()
	stations := a.stations.Get()
	selected := clamp(a.selected.Get(), 0, max(len(stations)-1, 0))

	if len(stations) == 0 {
		box.AddChild(gotui.New(
			gotui.WithText("No stations available."),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		))
		return box
	}

	start := 0
	maxRows := 12
	if selected >= maxRows {
		start = selected - maxRows + 1
	}
	end := min(start+maxRows, len(stations))

	for i := start; i < end; i++ {
		if i == selected {
			row := gotui.New(
				gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
				gotui.WithGap(1),
			)
			row.AddChild(gotui.New(
				gotui.WithText("▶"),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.Cyan).Bold()),
			))
			row.AddChild(gotui.New(
				gotui.WithText(compactText(stations[i].Title, 66)),
				gotui.WithTextGradient(gotui.NewGradient(gotui.BrightCyan, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
				gotui.WithTextStyle(gotui.NewStyle().Bold()),
			))
			box.AddChild(row)
		} else {
			box.AddChild(gotui.New(
				gotui.WithText(fmt.Sprintf("  %s", compactText(stations[i].Title, 68))),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
			))
		}
	}

	box.AddChild(gotui.New(gotui.WithHR()))
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("  %d / %d stations", selected+1, len(stations))),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))

	return box
}

func (a *app) renderResolving() *gotui.Element {
	box := contentBox()
	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]

	row := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(2),
	)
	row.AddChild(gotui.New(
		gotui.WithText(spin),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.Magenta)),
	))
	row.AddChild(gotui.New(
		gotui.WithText("Resolving stream URL..."),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite)),
	))
	box.AddChild(row)
	box.AddChild(gotui.New(
		gotui.WithText("  This may take a few seconds depending on network."),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))
	return box
}

func (a *app) renderPlayer() *gotui.Element {
	box := contentBox()
	station := a.currentStation.Get()
	isPaused := a.paused.Get()

	// === Top: album art + info panel side-by-side ===
	topRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(2),
	)

	// ASCII album art panel
	artColor := gotui.NewGradient(gotui.Cyan, gotui.Magenta).At(
		(math.Sin(a.pulsePhase.Get()) + 1) / 2,
	)
	artLines := []string{
		"╭───────╮",
		"│  ◉◉◉  │",
		"│ ◉   ◉ │",
		"│  ◉◉◉  │",
		"╰───────╯",
	}
	artBox := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithGap(0),
	)
	for _, line := range artLines {
		artBox.AddChild(gotui.New(
			gotui.WithText(line),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(artColor)),
		))
	}
	topRow.AddChild(artBox)

	// Info panel
	infoBox := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithGap(1),
	)
	infoBox.AddChild(gotui.New(
		gotui.WithText(compactText(station.Title, 50)),
		gotui.WithTextGradient(gotui.NewGradient(gotui.BrightCyan, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
		gotui.WithTextStyle(gotui.NewStyle().Bold()),
	))
	// Status badge
	statusText := "▶  PLAYING"
	statusStyle := gotui.NewStyle().Foreground(gotui.BrightGreen).Bold()
	if isPaused {
		statusText = "⏸  PAUSED"
		statusStyle = gotui.NewStyle().Foreground(gotui.BrightYellow).Bold()
	}
	infoBox.AddChild(gotui.New(
		gotui.WithText(statusText),
		gotui.WithTextStyle(statusStyle),
	))
	infoBox.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("  Time   %s", a.playbackElapsed())),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	topRow.AddChild(infoBox)
	box.AddChild(topRow)

	box.AddChild(gotui.New(gotui.WithHR()))

	// === Wave visualizer ===
	wave := a.buildWaveVisualizer(isPaused)
	box.AddChild(wave)

	box.AddChild(gotui.New(gotui.WithHR()))

	// === Volume bar ===
	vol := a.volume.Get()
	volRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(1),
	)
	volRow.AddChild(gotui.New(
		gotui.WithText("  VOL"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))
	volRow.AddChild(gotui.New(
		gotui.WithText(renderFancyBar(int64(vol), 100, 28)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.Cyan)),
	))
	volRow.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%3d%%", vol)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite)),
	))
	box.AddChild(volRow)

	return box
}

// buildWaveVisualizer renders an animated ASCII frequency-bar visualizer.
func (a *app) buildWaveVisualizer(paused bool) *gotui.Element {
	phase := a.wavePhase.Get()
	numBars := 32
	barChars := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

	row := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(0),
	)

	for i := 0; i < numBars; i++ {
		var height float64
		if paused {
			height = 0.1 + 0.05*math.Sin(float64(i)*0.8)
		} else {
			// Multi-frequency superposition for organic look
			height = 0.5 +
				0.3*math.Sin(float64(i)*0.6+phase) +
				0.15*math.Sin(float64(i)*1.3+phase*1.7) +
				0.05*math.Sin(float64(i)*2.1+phase*2.3)
			if height < 0 {
				height = 0
			}
			if height > 1 {
				height = 1
			}
		}

		barIdx := int(height * float64(len(barChars)-1))
		if barIdx < 0 {
			barIdx = 0
		}
		if barIdx >= len(barChars) {
			barIdx = len(barChars) - 1
		}

		// Colour: hue shifts across bars + animated phase
		hue := math.Mod(float64(i)*11+phase*30, 360)
		r, g, b := hslToRGB(hue, 0.9, 0.55)
		barColor := gotui.RGBColor(r, g, b)

		row.AddChild(gotui.New(
			gotui.WithText(barChars[barIdx]),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(barColor)),
		))
	}
	return row
}

func (a *app) renderError() *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightRed)),
		gotui.WithPadding(1),
		gotui.WithGap(1),
	)
	box.AddChild(gotui.New(
		gotui.WithText("✖  Error"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightRed).Bold()),
	))
	box.AddChild(gotui.New(
		gotui.WithText(strings.TrimSpace(a.errMessage.Get())),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite)),
	))
	return box
}

func contentBox() *gotui.Element {
	return gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		gotui.WithPadding(1),
		gotui.WithGap(1),
	)
}

// renderFancyBar renders a Unicode block progress bar.
func renderFancyBar(current, total int64, width int) string {
	if width < 4 {
		width = 4
	}
	if total <= 0 {
		total = current
		if total <= 0 {
			total = 1
		}
	}
	current = clamp64(current, 0, total)
	filled := int((float64(current) / float64(total)) * float64(width))
	filled = clamp(filled, 0, width)
	empty := width - filled
	return "▕" + strings.Repeat("█", filled) + strings.Repeat("░", empty) + "▏"
}

// hslToRGB converts HSL (h in [0,360), s,l in [0,1]) to RGB bytes.
func hslToRGB(h, s, l float64) (uint8, uint8, uint8) {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	toB := func(v float64) uint8 { return uint8(math.Round((v + m) * 255)) }
	return toB(r), toB(g), toB(b)
}

func (a *app) modeLabel() string {
	switch a.mode.Get() {
	case viewBoot:
		return "Bootstrapping"
	case viewSelect:
		return "Station Selection"
	case viewResolving:
		return "Preparing Stream"
	case viewPlayer:
		return "Now Playing"
	case viewError:
		return "Error"
	default:
		return ""
	}
}

func (a *app) playbackElapsed() string {
	startedAt := a.playStartedAt.Get()
	if startedAt.IsZero() {
		return "00:00"
	}

	elapsed := a.now.Get().Sub(startedAt) - a.totalPaused.Get()
	if a.paused.Get() {
		pausedAt := a.pausedStartedAt.Get()
		if !pausedAt.IsZero() {
			elapsed = pausedAt.Sub(startedAt) - a.totalPaused.Get()
		}
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

	current = clamp64(current, 0, total)
	filled := int((float64(current) / float64(total)) * float64(width))
	filled = clamp(filled, 0, width)

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

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clamp64(value, minValue, maxValue int64) int64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var (
	_ gotui.AppBinder       = (*app)(nil)
	_ gotui.KeyListener     = (*app)(nil)
	_ gotui.WatcherProvider = (*app)(nil)
)
