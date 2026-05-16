package tui

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	gotui "github.com/grindlemire/go-tui"
	"github.com/kidixdev/lofi-radio/internal/bootstrap"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
)

type viewMode int

const (
	viewBoot viewMode = iota
	viewChannelSelect
	viewSelect
	viewResolving
	viewPlayer
	viewError
)

const (
	channelHint = "Enter select channel  Up/Down or k/j navigate  q quit"
	selectHint  = "Enter play  Up/Down or k/j navigate  s channels  q quit"
	playerHint  = "Space pause/resume  +/- volume  s categories  c channels  q quit"
)

type asyncKind string

const (
	asyncBootstrap       asyncKind = "bootstrap"
	asyncFetchCategories asyncKind = "fetch_categories"
	asyncResolve         asyncKind = "resolve"
	asyncPlay            asyncKind = "play"
)

type asyncResult struct {
	kind         asyncKind
	categories   []radio.Category
	category     radio.Category
	streamURL    string
	resolveToken int
	err          error
}

type app struct {
	channelName        string
	playlistURL        string
	playingChannelURL  string
	playingChannelName string
	channels           []config.Channel

	mode       *gotui.State[viewMode]
	status     *gotui.State[string]
	errMessage *gotui.State[string]
	footerHint *gotui.State[string]
	categories *gotui.State[[]radio.Category]
	selected   *gotui.State[int]
	bootEvent  *gotui.State[bootstrap.ProgressEvent]

	player          *radio.Player
	currentCategory *gotui.State[radio.Category]
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
	vizLiveHold  int

	// vizBands holds the smoothed per-band amplitudes updated by onTick.
	// Plain array (not State) — only accessed from the UI goroutine.
	vizBands [radio.NumBands]float64

	resolveToken *gotui.State[int]
	bootCh       chan bootstrap.ProgressEvent
	resultCh     chan asyncResult

	exitErr error
	fatal   bool
}

func Run(channelName, playlistURL string) error {
	component := newApp(channelName, playlistURL)

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

func newApp(channelName, playlistURL string) *app {
	component := &app{
		channelName: channelName,
		playlistURL: playlistURL,
		channels:    config.Channels(),

		mode:       gotui.NewState(viewBoot),
		status:     gotui.NewState("Checking dependencies"),
		errMessage: gotui.NewState(""),
		footerHint: gotui.NewState("Press q to quit"),
		categories: gotui.NewState([]radio.Category{}),
		selected:   gotui.NewState(0),
		bootEvent:  gotui.NewState(bootstrap.ProgressEvent{}),

		player:          radio.NewPlayer(55),
		currentCategory: gotui.NewState(radio.Category{}),
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
	a.categories.BindApp(ui)
	a.selected.BindApp(ui)
	a.bootEvent.BindApp(ui)
	a.currentCategory.BindApp(ui)
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
		gotui.OnTimer(33*time.Millisecond, a.onTick),
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
			ke.App().Stop()
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
		gotui.OnStop(gotui.Rune('s'), func(ke gotui.KeyEvent) {
			if a.mode.Get() == viewPlayer {
				a.goToSelector("Pick another category")
			} else if a.mode.Get() == viewSelect {
				a.goToChannelSelector()
			}
		}),
		gotui.OnStop(gotui.Rune('c'), func(ke gotui.KeyEvent) {
			if a.mode.Get() == viewPlayer || a.mode.Get() == viewSelect {
				a.goToChannelSelector()
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

		a.goToChannelSelector()

	case asyncFetchCategories:
		if result.err != nil {
			radio.LogErrorDetails(result.err)
			a.handleSwitchError(radio.UserMessage(result.err))
			return
		}

		if len(result.categories) == 0 {
			a.setTransientError("no available categories found for this channel")
			return
		}

		a.categories.Set(result.categories)
		a.selected.Set(0)
		a.mode.Set(viewSelect)
		a.status.Set("Select a category")
		a.footerHint.Set(selectHint)
		a.errMessage.Set("")
		a.fatal = false
		a.exitErr = nil

	case asyncResolve:
		if result.resolveToken != a.resolveToken.Get() {
			return
		}

		if result.err != nil {
			radio.Logf("ui.resolve.error err=%v", result.err)
			radio.LogErrorDetails(result.err)
			a.handleSwitchError(radio.UserMessage(result.err))
			return
		}

		// Start playback off the UI loop; ffmpeg probe can block for seconds.
		a.status.Set("Starting audio stream")
		go func(resolveToken int, st radio.Category, streamURL string) {
			defer func() {
				if recovered := recover(); recovered != nil {
					radio.Logf("ui.play.panic category=%q recovered=%v", st.Title, recovered)
					a.emitResult(asyncResult{
						kind:         asyncPlay,
						category:     st,
						resolveToken: resolveToken,
						err:          fmt.Errorf("play panic: %v", recovered),
					})
				}
			}()
			err := a.player.Play(streamURL)
			a.emitResult(asyncResult{
				kind:         asyncPlay,
				category:     st,
				resolveToken: resolveToken,
				err:          err,
			})
		}(result.resolveToken, result.category, result.streamURL)

	case asyncPlay:
		if result.resolveToken != a.resolveToken.Get() {
			return
		}
		if result.err != nil {
			radio.Logf("ui.play.error category=%q err=%v", result.category.Title, result.err)
			a.handleSwitchError("Playback failed. Please try another category.")
			return
		}

		now := time.Now()
		a.currentCategory.Set(result.category)
		a.playingChannelURL = a.playlistURL
		a.playingChannelName = a.channelName
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

	// Advance spinner every 3 ticks (~99ms @ 33ms tick -> ~10 FPS spinner).
	if a.aniTick%3 == 0 {
		a.spinnerFrame.Update(func(v int) int { return v + 1 })
	}
	a.wavePhase.Update(func(v float64) float64 { return v + 0.029 })
	a.pulsePhase.Update(func(v float64) float64 { return v + 0.0165 })

	if a.mode.Get() != viewPlayer || !a.playing.Get() {
		// Decay vizBands to zero when not in player mode.
		for b := range a.vizBands {
			a.vizBands[b] *= 0.85
		}
		a.vizLiveHold = 0
		return
	}

	// Consume playback completion/error signal to surface real failures.
	if waitCh := a.player.WaitChan(); waitCh != nil {
		select {
		case err, ok := <-waitCh:
			a.playing.Set(false)
			a.paused.Set(false)
			if !ok || err == nil {
				a.setTransientError("playback stopped")
			} else {
				a.handleSwitchError("Playback failed. Please try another category.")
			}
			return
		default:
		}
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

	// Analyzer restart is handled in radio.Player capture loop.
	// UI must not force restarts, which can interrupt otherwise healthy flow.

	// Keep "live spectrum" sticky across brief PCM analyzer gaps to avoid
	// rapid buffering/live toggling when audio output itself is stable.
	if a.paused.Get() {
		a.vizLiveHold = 0
	} else if a.player.Viz.IsActive() && a.player.Viz.IsFresh(1200*time.Millisecond) {
		a.vizLiveHold = 90 // ~3s at 33ms tick
	} else if a.vizLiveHold > 0 {
		a.vizLiveHold--
	}

	// --- Visualizer smoothing (wall-clock based, runs at steady 30 fps) ---
	// Sample the latest raw bands from the analysis goroutine.
	raw := a.player.Viz.Get()
	paused := a.paused.Get()
	fresh := a.player.Viz.IsFresh(180 * time.Millisecond)

	const (
		attackFresh = 0.36  // smooth rise to reduce frame-to-frame jitter
		attackStale = 0.12  // avoid sudden jumps when feed resumes after stalls
		decayFresh  = 0.93  // gentle release while stream is healthy
		decayStale  = 0.985 // very slow fall during short analyzer stalls
		decayPause  = 0.94  // slow decay when paused (visual idle)
	)

	for b := 0; b < radio.NumBands; b++ {
		target := raw[b]
		if paused {
			target = 0 // decay to flat when paused
		}
		attack := attackFresh
		decay := decayFresh
		if paused {
			attack = attackStale
			decay = decayPause
		} else if !fresh {
			attack = attackStale
			decay = decayStale
		}
		if target > a.vizBands[b] {
			a.vizBands[b] += attack * (target - a.vizBands[b])
		} else {
			a.vizBands[b] *= decay
		}
	}
}

func (a *app) handleQuitOrBack(ke gotui.KeyEvent) {
	switch a.mode.Get() {
	case viewPlayer:
		a.goToSelector("Select a category")
		return

	case viewSelect:
		a.goToChannelSelector()
		return

	case viewChannelSelect:
		if a.playing.Get() {
			a.mode.Set(viewPlayer)
			a.status.Set("Playing")
			a.footerHint.Set(playerHint)
			return
		}
		// Top level menu: do nothing on escape to prevent accidental exit
		return

	case viewResolving:
		a.resolveToken.Update(func(v int) int { return v + 1 })
		a.goToSelector("Select a category")
		return

	case viewError:
		if len(a.categories.Get()) > 0 && !a.fatal {
			a.goToSelector("Select a category")
			return
		}
		a.goToChannelSelector()
		return
	}
}

func (a *app) handleEnter(ke gotui.KeyEvent) {
	switch a.mode.Get() {
	case viewChannelSelect:
		if len(a.channels) == 0 {
			return
		}
		idx := clamp(a.selected.Get(), 0, len(a.channels)-1)
		a.selected.Set(idx)
		channel := a.channels[idx]

		// If this is already the current channel and we have categories, just go to selector
		if channel.PlaylistURL == a.playlistURL && len(a.categories.Get()) > 0 {
			a.goToSelector("Select a category")
			return
		}

		a.startFetchCategories(channel.Name, channel.PlaylistURL)

	case viewSelect:
		categories := a.categories.Get()
		if len(categories) == 0 {
			return
		}

		selected := clamp(a.selected.Get(), 0, len(categories)-1)
		a.selected.Set(selected)
		category := categories[selected]

		// Smart Navigation: if already playing this category, just go back to player.
		if a.playing.Get() && a.currentCategory.Get().VideoURL == category.VideoURL {
			a.mode.Set(viewPlayer)
			a.status.Set("Playing")
			a.footerHint.Set(playerHint)
			return
		}

		a.startResolve(category)

	case viewError:
		a.handleQuitOrBack(ke)
	}
}

func (a *app) moveSelection(delta int) {
	if a.mode.Get() != viewSelect && a.mode.Get() != viewChannelSelect {
		return
	}

	var maxIdx int
	if a.mode.Get() == viewSelect {
		maxIdx = len(a.categories.Get()) - 1
	} else {
		maxIdx = len(a.channels) - 1
	}

	if maxIdx < 0 || delta == 0 {
		return
	}

	a.selected.Set(clamp(a.selected.Get()+delta, 0, maxIdx))
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
	if a.mode.Get() != viewPlayer && a.mode.Get() != viewSelect && a.mode.Get() != viewResolving && a.mode.Get() != viewError && a.mode.Get() != viewChannelSelect {
		return
	}

	if len(a.categories.Get()) == 0 {
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

func (a *app) goToChannelSelector() {
	// Find current channel index to pre-select it
	initialIdx := 0
	for i, c := range a.channels {
		if c.PlaylistURL == a.playlistURL {
			initialIdx = i
			break
		}
	}

	a.selected.Set(initialIdx)
	a.mode.Set(viewChannelSelect)
	a.status.Set("Select a channel")
	a.footerHint.Set(channelHint)
	a.errMessage.Set("")
	a.fatal = false
	a.exitErr = nil
}

func (a *app) setTransientError(message string) {
	a.mode.Set(viewError)
	a.errMessage.Set(message)
	a.footerHint.Set("Press q or Enter to return")
	a.fatal = false
	a.exitErr = nil
}

func (a *app) handleSwitchError(message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		trimmed = "Operation failed. Please try another category."
	}

	if a.playing.Get() {
		a.mode.Set(viewPlayer)
		a.status.Set(trimmed + " Continuing current playback.")
		a.footerHint.Set(playerHint)
		return
	}

	a.setTransientError(trimmed)
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
		defer func() {
			if recovered := recover(); recovered != nil {
				radio.Logf("ui.bootstrap.panic recovered=%v", recovered)
				a.emitResult(asyncResult{kind: asyncBootstrap, err: fmt.Errorf("bootstrap panic: %v", recovered)})
			}
		}()
		reporter := bootstrap.ProgressReporterFunc(func(event bootstrap.ProgressEvent) {
			select {
			case a.bootCh <- event:
			default:
			}
		})

		if _, err := bootstrap.EnsureDependenciesWithProgress(reporter); err != nil {
			radio.Logf("ui.bootstrap.error stage=dependencies err=%v", err)
			a.emitResult(asyncResult{kind: asyncBootstrap, err: err})
			return
		}

		a.emitResult(asyncResult{kind: asyncBootstrap, err: nil})
	}()
}

func (a *app) startFetchCategories(channelName, playlistURL string) {
	a.channelName = channelName
	a.playlistURL = playlistURL
	a.mode.Set(viewBoot)
	a.status.Set(fmt.Sprintf("Connecting to %s", channelName))

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				radio.Logf("ui.fetch_categories.panic recovered=%v", recovered)
				a.emitResult(asyncResult{kind: asyncFetchCategories, err: fmt.Errorf("fetch categories panic: %v", recovered)})
			}
		}()

		categories, err := radio.FetchCategoriesFromPlaylist(playlistURL)
		if err != nil {
			radio.Logf("ui.fetch_categories.error channel=%s err=%v", channelName, err)
		}
		a.emitResult(asyncResult{
			kind:       asyncFetchCategories,
			categories: categories,
			err:        err,
		})
	}()
}

func (a *app) startResolve(category radio.Category) {
	token := a.resolveToken.Get() + 1
	a.resolveToken.Set(token)
	a.mode.Set(viewResolving)
	a.status.Set("Resolving direct stream URL")
	a.footerHint.Set("Press q to cancel")
	a.errMessage.Set("")

	go func(resolveToken int, st radio.Category) {
		defer func() {
			if recovered := recover(); recovered != nil {
				radio.Logf("ui.resolve.panic category=%q recovered=%v", st.Title, recovered)
				a.emitResult(asyncResult{
					kind:         asyncResolve,
					category:     st,
					resolveToken: resolveToken,
					err:          fmt.Errorf("resolve panic: %v", recovered),
				})
			}
		}()
		streamURL, err := radio.GetDirectAudioURL(st.VideoURL)
		if err != nil {
			radio.Logf("ui.resolve.error category=%q err=%v", st.Title, err)
		}
		a.emitResult(asyncResult{
			kind:         asyncResolve,
			category:     st,
			streamURL:    streamURL,
			resolveToken: resolveToken,
			err:          err,
		})
	}(token, category)
}

func (a *app) emitResult(result asyncResult) {
	select {
	case a.resultCh <- result:
	default:
	}
}

func (a *app) Render(ui *gotui.App) *gotui.Element {
	termWidth, termHeight := ui.Size()
	rootVerticalPadding := 1
	rootHorizontalPadding := 2
	rootGap := 1
	if termHeight < 30 {
		rootVerticalPadding = 0
	}
	if termWidth < 110 {
		rootHorizontalPadding = 1
	}
	if termHeight < 24 {
		rootGap = 0
	}
	contentHeight := availableContentHeight(termHeight, rootVerticalPadding, rootGap)

	// Refined pulsing color for the core brand/borders.
	pulse := a.pulsePhase.Get()
	t := (math.Sin(pulse) + 1) / 2
	// Sleek sunset: Pinkish-Red to Bright Yellow
	borderColor := gotui.NewGradient(gotui.RGBColor(255, 40, 100), gotui.RGBColor(255, 200, 40)).At(t)

	root := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithHeight(termHeight),
		gotui.WithJustify(gotui.JustifyStart),
		gotui.WithGap(rootGap),
		gotui.WithPaddingTRBL(rootVerticalPadding, rootHorizontalPadding, rootVerticalPadding, rootHorizontalPadding),
	)

	root.AddChild(a.renderHeader(borderColor))
	var mainView *gotui.Element
	switch a.mode.Get() {
	case viewBoot:
		mainView = a.renderBoot()
	case viewChannelSelect:
		mainView = a.renderChannelSelector(termWidth, termHeight, contentHeight)
	case viewSelect:
		mainView = a.renderSelector(termWidth, termHeight, contentHeight)
	case viewResolving:
		mainView = a.renderResolving()
	case viewPlayer:
		mainView = a.renderPlayer(termWidth)
	case viewError:
		mainView = a.renderError()
	}
	mainSlot := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithHeight(contentHeight),
		gotui.WithMinHeight(contentHeight),
		gotui.WithMaxHeight(contentHeight),
		gotui.WithFlexShrink(0),
		gotui.WithJustify(gotui.JustifyStart),
	)
	if mainView != nil {
		mainSlot.AddChild(mainView)
	}
	root.AddChild(mainSlot)

	// ── Footer ──────────────────────────────────────────────
	footer := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithHeight(3),
		gotui.WithMinHeight(3),
		gotui.WithMaxHeight(3),
		gotui.WithFlexShrink(0),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		gotui.WithPaddingTRBL(0, 2, 0, 2),
		gotui.WithAlign(gotui.AlignCenter),
	)
	footer.AddChild(gotui.New(
		gotui.WithText(" "+a.footerHint.Get()),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.White).Dim()),
	))
	root.AddChild(footer)

	return root
}

var spinnerBraille = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (a *app) renderHeader(borderColor gotui.Color) *gotui.Element {
	header := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithHeight(3),
		gotui.WithMinHeight(3),
		gotui.WithMaxHeight(3),
		gotui.WithFlexShrink(0),
		gotui.WithJustify(gotui.JustifySpaceBetween),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(borderColor)),
		gotui.WithPaddingTRBL(0, 2, 0, 2),
	)

	left := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithGap(1),
		gotui.WithFlexGrow(1),
	)

	// Pulsing intensity for the "RADIO" label
	pulse := a.pulsePhase.Get()
	glow := (math.Sin(pulse*1.8) + 1) / 2
	radioStyle := gotui.NewStyle().Bold()
	if glow > 0.7 {
		radioStyle = radioStyle.Foreground(gotui.BrightYellow)
	} else {
		radioStyle = radioStyle.Foreground(gotui.Yellow)
	}

	left.AddChild(gotui.New(
		gotui.WithText("LOFI"),
		gotui.WithWrap(false),
		gotui.WithTextGradient(gotui.NewGradient(gotui.White, gotui.BrightMagenta).WithDirection(gotui.GradientHorizontal)),
		gotui.WithTextStyle(gotui.NewStyle().Bold()),
	))
	left.AddChild(gotui.New(
		gotui.WithText("RADIO"),
		gotui.WithWrap(false),
		gotui.WithTextStyle(radioStyle),
	))
	left.AddChild(gotui.New(
		gotui.WithText("|"),
		gotui.WithWrap(false),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	left.AddChild(gotui.New(
		gotui.WithText(compactText(a.modeLabel(), 26)),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))

	right := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithGap(1),
		gotui.WithFlexShrink(0),
	)

	// Status Indicator
	statusText := "● LIVE"
	isLive := a.playing.Get() && !a.paused.Get()
	liveStyle := gotui.NewStyle().Bold()
	if isLive {
		// Faster pulse for the LIVE indicator
		if math.Sin(pulse*4.0) > 0 {
			liveStyle = liveStyle.Foreground(gotui.BrightRed)
		} else {
			liveStyle = liveStyle.Foreground(gotui.Red)
		}
	} else if a.paused.Get() {
		statusText = "● PAUSED"
		liveStyle = liveStyle.Foreground(gotui.BrightYellow)
	} else {
		statusText = "● IDLE"
		liveStyle = liveStyle.Foreground(gotui.BrightBlack).Dim()
	}

	right.AddChild(gotui.New(
		gotui.WithText(statusText),
		gotui.WithTextStyle(liveStyle),
	))

	right.AddChild(gotui.New(
		gotui.WithText("|"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))

	// System Clock
	right.AddChild(gotui.New(
		gotui.WithText(time.Now().Format("15:04:05")),
		gotui.WithWrap(false),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.White).Bold()),
	))

	header.AddChild(left)
	header.AddChild(right)
	return header
}

func (a *app) renderBoot() *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 40, 100))),
		gotui.WithPadding(2),
		gotui.WithFlexGrow(1),
		gotui.WithGap(1),
	)

	asciiArt := []string{
		` _      ___  ___ ___`,
		`| |    / _ \| __|_ _|`,
		`| |__ | (_) | _| | | `,
		`|____| \___/|_| |___|`,
	}
	box.AddChild(renderASCIIBlock(asciiArt))

	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]

	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%s  %s", spin, sanitizeBootstrapMessage(a.status.Get()))),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan)),
	))

	event := a.bootEvent.Get()
	if event.Type == bootstrap.ProgressEventDownload {
		label := "DOWNLOADING"
		totalLabel := "unknown"
		if event.Download.TotalBytes > 0 {
			totalLabel = humanBytes(event.Download.TotalBytes)
		}
		box.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("  %s  %s", label, renderFancyBar(event.Download.BytesReceived, event.Download.TotalBytes, 24))),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightYellow)),
		))
		box.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("  %s / %s   %s", humanBytes(event.Download.BytesReceived), totalLabel, humanSpeed(event.Download.SpeedPerSec))),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		))
	}
	return box
}

func (a *app) renderChannelSelector(termWidth, termHeight, contentHeight int) *gotui.Element {
	rowGap := 2
	if termWidth < 110 {
		rowGap = 1
	}

	row := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithJustify(gotui.JustifyStart),
		gotui.WithFlexGrow(1),
		gotui.WithGap(rowGap),
		gotui.WithMinHeight(0),
	)

	selected := clamp(a.selected.Get(), 0, max(len(a.channels)-1, 0))
	maxRows := selectorMaxRows(contentHeight)

	sidebarWidth := 40
	if termWidth >= 120 {
		sidebarWidth = 42
	} else if termWidth >= 100 {
		sidebarWidth = 38
	}

	ultraCompactSidebar := contentHeight < 18

	// Left: channel browser panel
	left := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithWidth(sidebarWidth),
		gotui.WithMinWidth(28),
		gotui.WithFlexShrink(0),
		gotui.WithMinHeight(0),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightCyan)),
		gotui.WithPadding(1),
		gotui.WithGap(0),
	)

	asciiArt := []string{
		`  ____ _   _    _    _   _ `,
		` / ___| | | |  / \  | \ | |`,
		`| |   | |_| | / _ \ |  \| |`,
		`| |___|  _  |/ ___ \| |\  |`,
		` \____|_| |_/_/   \_\_| \_|`,
	}
	if !ultraCompactSidebar {
		left.AddChild(renderASCIIBlock(asciiArt))
		left.AddChild(gotui.New(gotui.WithHR()))
	}
	left.AddChild(gotui.New(
		gotui.WithText(" CHANNEL BROWSER"),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))
	left.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" Total: %d", len(a.channels))),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	if len(a.channels) > 0 {
		left.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf(" Selected: %d/%d", selected+1, len(a.channels))),
			gotui.WithWrap(false),
			gotui.WithTruncate(true),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
	}

	// Right: channel list
	right := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithMinWidth(0),
		gotui.WithMinHeight(0),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.Cyan)),
		gotui.WithPadding(1),
		gotui.WithGap(0),
	)

	right.AddChild(gotui.New(
		gotui.WithText(" AVAILABLE CHANNELS"),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))
	right.AddChild(gotui.New(gotui.WithHR()))

	if len(a.channels) == 0 {
		right.AddChild(gotui.New(
			gotui.WithText(" No channels available."),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		))
	} else {
		start := 0
		if selected >= maxRows {
			start = selected - maxRows + 1
		}
		end := min(start+maxRows, len(a.channels))

		for i := start; i < end; i++ {
			channel := a.channels[i]
			isCurrent := a.playing.Get() && channel.PlaylistURL == a.playingChannelURL
			isSelected := i == selected

			r := gotui.New(
				gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
				gotui.WithGap(1),
				gotui.WithAlign(gotui.AlignCenter),
			)

			if isSelected {
				pulse := a.pulsePhase.Get()
				arrow := "▶"
				if math.Sin(pulse*2.5) > 0 {
					arrow = "▷"
				}
				r.AddChild(gotui.New(
					gotui.WithText(arrow),
					gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan).Bold()),
				))

				titleStyle := gotui.NewStyle().Bold()
				if isCurrent {
					titleStyle = titleStyle.Foreground(gotui.BrightGreen)
				}

				r.AddChild(gotui.New(
					gotui.WithText(compactText(channel.Name, 48)),
					gotui.WithWrap(false),
					gotui.WithTruncate(true),
					gotui.WithTextGradient(gotui.NewGradient(gotui.Cyan, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
					gotui.WithTextStyle(titleStyle),
				))
			} else {
				dim := gotui.NewStyle().Foreground(gotui.BrightBlack)
				if isCurrent {
					dim = dim.Foreground(gotui.Green).Dim()
				}
				r.AddChild(gotui.New(
					gotui.WithText(fmt.Sprintf("  %s", compactText(channel.Name, 50))),
					gotui.WithWrap(false),
					gotui.WithTruncate(true),
					gotui.WithTextStyle(dim),
				))
			}

			if isCurrent {
				r.AddChild(gotui.New(
					gotui.WithText(" [ACTIVE]"),
					gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightGreen).Bold().Dim()),
				))
			}

			right.AddChild(r)
		}

		if len(a.channels) > maxRows {
			right.AddChild(gotui.New(gotui.WithHR()))
			right.AddChild(gotui.New(
				gotui.WithText(fmt.Sprintf("  — %d/%d —", selected+1, len(a.channels))),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
			))
		}
	}

	row.AddChild(left)
	row.AddChild(right)
	return row
}

func (a *app) renderSelector(termWidth, termHeight, contentHeight int) *gotui.Element {
	rowGap := 2
	if termWidth < 110 {
		rowGap = 1
	}

	row := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithJustify(gotui.JustifyStart),
		gotui.WithFlexGrow(1),
		gotui.WithGap(rowGap),
		gotui.WithMinHeight(0),
	)

	categories := a.categories.Get()
	selected := clamp(a.selected.Get(), 0, max(len(categories)-1, 0))
	maxRows := selectorMaxRows(contentHeight)

	sidebarWidth := 40
	if termWidth >= 120 {
		sidebarWidth = 42
	} else if termWidth >= 100 {
		sidebarWidth = 38
	}

	ultraCompactSidebar := contentHeight < 18

	// Left: category browser panel
	left := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithWidth(sidebarWidth),
		gotui.WithMinWidth(28),
		gotui.WithFlexShrink(0),
		gotui.WithMinHeight(0),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.Magenta)),
		gotui.WithPadding(1),
		gotui.WithGap(0),
	)

	asciiArt := []string{
		` _      ___  ___ ___`,
		`| |    / _ \| __|_ _|`,
		`| |__ | (_) | _| | | `,
		`|____| \___/|_| |___|`,
	}
	if !ultraCompactSidebar {
		left.AddChild(renderASCIIBlock(asciiArt))
		left.AddChild(gotui.New(gotui.WithHR()))
	}
	left.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" %s", strings.ToUpper(a.channelName))),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))
	left.AddChild(gotui.New(
		gotui.WithText(" CATEGORY BROWSER"),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	left.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" Total: %d", len(categories))),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	if len(categories) > 0 {
		left.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf(" Selected: %d/%d", selected+1, len(categories))),
			gotui.WithWrap(false),
			gotui.WithTruncate(true),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
	}

	// Right: category list
	right := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithMinWidth(0),
		gotui.WithMinHeight(0),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.Yellow)),
		gotui.WithPadding(1),
		gotui.WithGap(0),
	)

	right.AddChild(gotui.New(
		gotui.WithText(" AVAILABLE CATEGORIES"),
		gotui.WithWrap(false),
		gotui.WithTruncate(true),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))
	right.AddChild(gotui.New(gotui.WithHR()))

	if len(categories) == 0 {
		right.AddChild(gotui.New(
			gotui.WithText(" No categories available."),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		))
	} else {
		start := 0
		if selected >= maxRows {
			start = selected - maxRows + 1
		}
		end := min(start+maxRows, len(categories))

		for i := start; i < end; i++ {
			category := categories[i]
			isCurrent := a.playing.Get() && a.currentCategory.Get().VideoURL == category.VideoURL
			isSelected := i == selected

			r := gotui.New(
				gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
				gotui.WithGap(1),
				gotui.WithAlign(gotui.AlignCenter),
			)

			if isSelected {
				// Selection indicator: Pulsing arrow
				pulse := a.pulsePhase.Get()
				arrow := "▶"
				if math.Sin(pulse*2.5) > 0 {
					arrow = "▷"
				}
				r.AddChild(gotui.New(
					gotui.WithText(arrow),
					gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.Red).Bold()),
				))

				titleStyle := gotui.NewStyle().Bold()
				if isCurrent {
					titleStyle = titleStyle.Foreground(gotui.BrightGreen)
				}

				r.AddChild(gotui.New(
					gotui.WithText(compactText(category.Title, 48)),
					gotui.WithWrap(false),
					gotui.WithTruncate(true),
					gotui.WithTextGradient(gotui.NewGradient(gotui.Yellow, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
					gotui.WithTextStyle(titleStyle),
				))
			} else {
				dim := gotui.NewStyle().Foreground(gotui.BrightBlack)
				if isCurrent {
					dim = dim.Foreground(gotui.Green).Dim()
				}
				r.AddChild(gotui.New(
					gotui.WithText(fmt.Sprintf("  %s", compactText(category.Title, 50))),
					gotui.WithWrap(false),
					gotui.WithTruncate(true),
					gotui.WithTextStyle(dim),
				))
			}

			if isCurrent {
				r.AddChild(gotui.New(
					gotui.WithText(" [PLAYING]"),
					gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightGreen).Bold().Dim()),
				))
			}

			right.AddChild(r)
		}

		if len(categories) > maxRows {
			right.AddChild(gotui.New(gotui.WithHR()))
			right.AddChild(gotui.New(
				gotui.WithText(fmt.Sprintf("  — %d/%d —", selected+1, len(categories))),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
			))
		}
	}

	row.AddChild(left)
	row.AddChild(right)
	return row
}

func (a *app) renderResolving() *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 40, 100))),
		gotui.WithPaddingTRBL(1, 2, 1, 2),
		gotui.WithFlexGrow(1),
		gotui.WithGap(0),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithJustify(gotui.JustifyCenter),
	)

	// Get resolving info
	categories := a.categories.Get()
	selected := a.selected.Get()
	categoryTitle := "Unknown Station"
	if selected >= 0 && selected < len(categories) {
		categoryTitle = compactText(categories[selected].Title, 64)
	}

	// 1. ASCII Header
	box.AddChild(renderASCIIBlock([]string{
		` _____ _   _ _   _ ___ _   _  ____ `,
		`|_   _| | | | \ | |_ _| \ | |/ ___|`,
		`  | | | | | |  \| || ||  \| | |  _ `,
		`  | | | |_| | |\  || || |\  | |_| |`,
		`  |_|  \___/|_| \_|___|_| \_|\____|`,
	}))

	box.AddChild(gotui.New(gotui.WithHeight(1)))

	// 2. Station Info
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("CHANNEL : %s", strings.ToUpper(a.channelName))),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("STATION : %s", strings.ToUpper(categoryTitle))),
		gotui.WithTextStyle(gotui.NewStyle().Bold().Foreground(gotui.BrightWhite)),
	))

	box.AddChild(gotui.New(gotui.WithHeight(1)))

	// 3. Pulsing signal animation
	pulse := a.pulsePhase.Get()
	animRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(2),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithHeight(1),
	)
	for i := 0; i < 7; i++ {
		dist := math.Abs(float64(i) - 3)
		active := math.Sin(pulse*6.0-dist*1.5) > 0.4
		char := "⠂"
		style := gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()
		if active {
			char = "⦿"
			style = gotui.NewStyle().Foreground(gotui.BrightMagenta).Bold()
		}
		animRow.AddChild(gotui.New(
			gotui.WithText(char),
			gotui.WithTextStyle(style),
		))
	}
	box.AddChild(animRow)

	box.AddChild(gotui.New(gotui.WithHeight(1)))

	// 4. Status
	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%s  CONNECTING TO SERVER...", spin)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.Magenta)),
	))

	return box
}

func (a *app) renderPlayer(termWidth int) *gotui.Element {
	// Root row container
	row := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithFlexGrow(1),
		gotui.WithGap(1),
	)

	category := a.currentCategory.Get()
	isPaused := a.paused.Get()

	// LEFT COLUMN (Info & Controls - 40% width)
	leftCol := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithWidth(int(float64(termWidth)*0.38)),
		gotui.WithMinWidth(42),
		gotui.WithFlexShrink(0),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		gotui.WithPaddingTRBL(1, 2, 1, 2),
		gotui.WithGap(0),
	)

	// Consistent Indentation
	indent := gotui.WithPaddingTRBL(0, 1, 0, 0)

	// 1. Station Section
	leftCol.AddChild(gotui.New(
		gotui.WithText("● STATION"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold()),
	))

	maxTitleLen := 32
	if termWidth > 130 {
		maxTitleLen = 42
	}

	leftCol.AddChild(gotui.New(
		gotui.WithText(compactText(category.Title, maxTitleLen)),
		gotui.WithTextGradient(gotui.NewGradient(gotui.Yellow, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
		gotui.WithTextStyle(gotui.NewStyle().Bold()),
		indent,
	))
	leftCol.AddChild(gotui.New(
		gotui.WithText(strings.ToUpper(a.playingChannelName)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightMagenta).Bold().Dim()),
		indent,
	))

	// 2. Playback Section
	leftCol.AddChild(gotui.New(gotui.WithHR()))
	leftCol.AddChild(gotui.New(
		gotui.WithText("● STATUS"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold()),
	))

	infoRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(2),
		gotui.WithAlign(gotui.AlignCenter),
		indent,
	)

	vinylFrames := []string{"◐", "◓", "◑", "◒"}
	vinyl := "⊙"
	if !isPaused {
		vinyl = vinylFrames[a.spinnerFrame.Get()%len(vinylFrames)]
	}
	infoRow.AddChild(gotui.New(
		gotui.WithText(vinyl),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan).Bold()),
	))

	statusText := "PLAYING"
	statusStyle := gotui.NewStyle().Foreground(gotui.BrightGreen)
	if isPaused {
		statusText = "PAUSED "
		statusStyle = gotui.NewStyle().Foreground(gotui.BrightYellow)
	}
	infoRow.AddChild(gotui.New(
		gotui.WithText(statusText),
		gotui.WithTextStyle(statusStyle.Bold()),
	))
	infoRow.AddChild(gotui.New(
		gotui.WithText(a.playbackElapsed()),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))
	leftCol.AddChild(infoRow)

	// Signal & Quality
	sigRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(2),
		indent,
	)
	sigBars := []string{" ", "▂", "▃", "▅", "▆", "█"}
	sigIdx := 3 + int(math.Sin(a.pulsePhase.Get()*3.0)*2.0)
	sigIdx = clamp(sigIdx, 1, 5)
	sigRow.AddChild(gotui.New(
		gotui.WithText("SIG "+sigBars[sigIdx]),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	sigRow.AddChild(gotui.New(
		gotui.WithText("192KBPS"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))
	leftCol.AddChild(sigRow)

	// 3. Audio Section
	leftCol.AddChild(gotui.New(gotui.WithHR()))
	leftCol.AddChild(gotui.New(
		gotui.WithText("● VOLUME"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold()),
	))

	vol := a.volume.Get()
	volRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(1),
		gotui.WithAlign(gotui.AlignCenter),
		indent,
	)
	volRow.AddChild(gotui.New(
		gotui.WithText(renderFancyBar(int64(vol), 100, 16)),
		gotui.WithTextGradient(gotui.NewGradient(gotui.RGBColor(140, 60, 255), gotui.RGBColor(255, 60, 140)).WithDirection(gotui.GradientHorizontal)),
		gotui.WithTextStyle(gotui.NewStyle().Bold()),
	))
	volRow.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%d%%", vol)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))
	leftCol.AddChild(volRow)

	// RIGHT COLUMN (Visuals - 60% width)
	rightCol := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithGap(0),
	)

	// Top: Background Decor
	decorBox := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		gotui.WithPaddingTRBL(0, 2, 0, 2),
	)

	decorBox.AddChild(gotui.New(
		gotui.WithText("● SIGNAL OSCILLOSCOPE"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold()),
	))

	// Create a simple reactive sine wave for the oscilloscope
	waveRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithJustify(gotui.JustifyCenter),
	)

	waveStr := ""
	pulse := a.pulsePhase.Get()
	width := (termWidth - 42) - 10
	if width > 0 {
		for x := 0; x < width; x++ {
			y := math.Sin(float64(x)*0.2 + pulse*10.0)
			if y > 0.6 {
				waveStr += "⬔"
			} else if y < -0.6 {
				waveStr += "⬕"
			} else {
				waveStr += "─"
			}
		}
	}
	waveRow.AddChild(gotui.New(
		gotui.WithText(waveStr),
		gotui.WithTextGradient(gotui.NewGradient(gotui.RGBColor(50, 100, 255), gotui.RGBColor(50, 255, 100))),
	))

	// Add some technical labels to the decor box
	statsRow := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithGap(4),
	)
	statsRow.AddChild(gotui.New(
		gotui.WithText("SOURCE: REMOTE/TCP"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))
	statsRow.AddChild(gotui.New(
		gotui.WithText("ENCODER: LAME MP3"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))
	statsRow.AddChild(gotui.New(
		gotui.WithText("BUFFER: 2500MS"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))

	decorBox.AddChild(waveRow)
	decorBox.AddChild(statsRow)

	// Bottom: Wave Visualizer
	vizHeight := 7
	vizRows := 4
	if termWidth >= 145 {
		vizHeight = 8
		vizRows = 5
	}
	visualizerBox := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithHeight(vizHeight),
		gotui.WithMinHeight(vizHeight),
		gotui.WithMaxHeight(vizHeight),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		gotui.WithPaddingTRBL(0, 1, 0, 1),
	)

	visualizerBox.AddChild(a.buildWaveVisualizer(isPaused, termWidth, vizRows))

	rightCol.AddChild(decorBox)
	rightCol.AddChild(visualizerBox)

	row.AddChild(leftCol)
	row.AddChild(rightCol)

	return row
}

// buildWaveVisualizer renders a real audio-reactive multi-row spectrum analyzer.
// a.vizBands contains the smoothed band amplitudes, updated every onTick (30fps).
// Falls back to an animated sine-wave placeholder while buffering.
func (a *app) buildWaveVisualizer(paused bool, termWidth, rows int) *gotui.Element {
	phase := a.wavePhase.Get()
	numBars := radio.NumBands
	if termWidth >= 160 {
		numBars = 48
	} else if termWidth >= 125 {
		numBars = 40
	}

	// Show buffering only until first analyzer frame is observed.
	// After data exists, keep live spectrum even across brief gaps.
	hasRealData := !paused && (a.player.Viz.HasData() || a.vizLiveHold > 0)

	// Compute per-bar display height.
	heights := make([]float64, numBars)
	for i := 0; i < numBars; i++ {
		src := (i * radio.NumBands) / numBars
		if src >= radio.NumBands {
			src = radio.NumBands - 1
		}
		if paused {
			heights[i] = a.vizBands[src] // decayed by onTick
		} else if hasRealData {
			heights[i] = a.vizBands[src]
		} else {
			// Animated sine-wave placeholder while buffering.
			h := 0.45 +
				0.30*math.Sin(float64(i)*0.55+phase) +
				0.15*math.Sin(float64(i)*1.2+phase*1.6) +
				0.08*math.Sin(float64(i)*2.0+phase*2.2)
			if h < 0 {
				h = 0
			}
			if h > 1 {
				h = 1
			}
			heights[i] = h
		}
	}

	// Block characters used for each row's fill level.
	// Each row represents 1/vizRows of the full height.
	// sub-row fill chars: 0=empty, 1-8=partial bottom→top.
	subFill := []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

	col := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithJustify(gotui.JustifyEnd),
		gotui.WithGap(0),
	)

	// Build spectrum rows top→bottom. Row 0 is the top.
	for row := 0; row < rows; row++ {
		hLine := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(0),
		)
		rowLo := float64(rows-1-row) / float64(rows)
		rowHi := float64(rows-row) / float64(rows)

		for i := 0; i < numBars; i++ {
			h := heights[i]
			var ch string
			if h <= rowLo {
				// Bar doesn't reach this row.
				ch = " "
			} else if h >= rowHi {
				// Full block.
				ch = "█"
			} else {
				// Partial: how far into this row.
				frac := (h - rowLo) * float64(rows)
				idx := clamp(int(frac*float64(len(subFill)-1)), 0, len(subFill)-1)
				ch = subFill[idx]
			}

			// Hue: low bars cyan→blue, high bars yellow→red.
			// Smooth sunset gradient: Cyan -> Blue -> Purple -> Red
			barFrac := float64(i) / float64(numBars)
			rowFrac := float64(rows-1-row) / float64(rows)

			// Color calculation for a more dynamic look
			hue := 180 + barFrac*120 + rowFrac*60
			hue = math.Mod(hue+phase*8, 360)
			if hue < 0 {
				hue += 360
			}
			sat := 0.75 + 0.25*h
			lit := 0.40 + 0.25*h
			if ch == " " {
				lit = 0.06 // dim background
				sat = 0.1
				ch = "·"
			}
			r, g, b := hslToRGB(hue, sat, lit)

			hLine.AddChild(gotui.New(
				gotui.WithText(ch),
				gotui.WithFlexGrow(1),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.RGBColor(r, g, b))),
			))
		}
		col.AddChild(hLine)
	}

	return col
}

func (a *app) buildVisualizerDecor(paused bool, termWidth int) *gotui.Element {
	phase := a.pulsePhase.Get()
	rows := 5
	if termWidth < 110 {
		rows = 4
	}
	cols := 34
	if termWidth >= 150 {
		cols = 42
	} else if termWidth < 115 {
		cols = 28
	}

	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithJustify(gotui.JustifyCenter),
		gotui.WithGap(0),
	)

	for r := 0; r < rows; r++ {
		line := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(0),
		)
		rowFrac := float64(r) / float64(max(rows-1, 1))
		for c := 0; c < cols; c++ {
			x := float64(c) / float64(max(cols-1, 1))
			// More complex wave for the background
			w := 0.5 + 0.4*math.Sin(phase*1.4+x*9.0-rowFrac*2.3) + 0.1*math.Sin(phase*3.1-x*4.5)

			ch := "·"
			if w > 0.85 {
				ch = "•"
			}
			if paused && w > 0.92 {
				ch = "◦"
			}

			// Pulsing colors: Deep Navy to Soft Teal/Violet
			hue := 240 + 40*math.Sin(phase*0.5) + 20*rowFrac + 20*w
			sat := 0.30 + 0.20*w
			lit := 0.10 + 0.10*w

			r8, g8, b8 := hslToRGB(hue, sat, lit)
			line.AddChild(gotui.New(
				gotui.WithText(ch),
				gotui.WithFlexGrow(1),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.RGBColor(r8, g8, b8))),
			))
		}
		box.AddChild(line)
	}

	return box
}

func (a *app) renderError() *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithPaddingTRBL(1, 0, 0, 0),
		gotui.WithGap(1),
	)
	box.AddChild(gotui.New(
		gotui.WithText("  ✖  "+strings.TrimSpace(a.errMessage.Get())),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightRed).Bold()),
	))
	return box
}

func renderASCIIBlock(lines []string) *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex),
		gotui.WithDirection(gotui.Column),
		gotui.WithGap(0),
	)
	for _, line := range lines {
		box.AddChild(gotui.New(
			gotui.WithText(line),
			gotui.WithWrap(false),
			gotui.WithTextGradient(gotui.NewGradient(gotui.Yellow, gotui.Red).WithDirection(gotui.GradientHorizontal)),
			gotui.WithTextStyle(gotui.NewStyle().Bold()),
		))
	}
	return box
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
	case viewChannelSelect:
		return "Channel Selection"
	case viewSelect:
		return "Category Selection"
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

func sanitizeBootstrapMessage(msg string) string {
	low := strings.ToLower(msg)
	if strings.Contains(low, "downloading binary") || strings.Contains(low, "downloading ffmpeg bundle") {
		return "Downloading required dependency"
	}
	if strings.Contains(low, "binary downloaded") {
		return "Dependency ready"
	}
	if strings.Contains(low, "using system binary") || strings.Contains(low, "using local cached binary") {
		return "Initializing components"
	}
	return msg
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

func availableContentHeight(termHeight, rootVerticalPadding, rootGap int) int {
	// Header/footer each render as a 3-row rounded box.
	const headerHeight = 3
	const footerHeight = 3
	chrome := (rootVerticalPadding * 2) + (rootGap * 2) + headerHeight + footerHeight
	// Be more conservative to avoid bottom-row clipping or overlaps.
	return max(termHeight-chrome-2, 8)
}

func selectorMaxRows(contentHeight int) int {
	// Rows available in right panel after border/padding/title+divider and selection meter.
	rows := contentHeight - 9
	return clamp(rows, 3, 14)
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

var (
	_ gotui.AppBinder       = (*app)(nil)
	_ gotui.KeyListener     = (*app)(nil)
	_ gotui.WatcherProvider = (*app)(nil)
)
