package tui

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	gotui "github.com/grindlemire/go-tui"
	"github.com/kidixdev/lofi-radio/internal/bootstrap"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"github.com/kidixdev/lofi-radio/internal/update"
	"github.com/kidixdev/lofi-radio/internal/version"
)

type viewMode int

const (
	viewBoot viewMode = iota
	viewChannelSelect
	viewSelect
	viewResolving
	viewPlayer
	viewUpdatePrompt
	viewUpdating
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
	asyncCheckUpdate     asyncKind = "check_update"
	asyncRunUpdate       asyncKind = "run_update"
)

type asyncResult struct {
	kind         asyncKind
	categories   []radio.Category
	category     radio.Category
	streamURL    string
	resolveToken int
	release      update.ReleaseInfo
	hasUpdate    bool
	installPath  string
	err          error
}

type app struct {
	channelName        string
	playlistURL        string
	playingChannelURL  string
	playingChannelName string
	channels           []config.Channel
	configManager      *config.Manager

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

	// vizBands holds the smoothed per-band amplitudes updated by onTick.
	vizBands    [radio.NumBands]float64
	vizHistory  [][4]float64
	vizLiveHold int
	listOffset  int
	updateDone  bool

	resolveToken *gotui.State[int]
	updateChoice *gotui.State[int]
	bootCh       chan bootstrap.ProgressEvent
	resultCh     chan asyncResult

	updateRelease update.ReleaseInfo
	updateError   string

	persistMu            sync.Mutex
	persistTimer         *time.Timer
	persistPendingVolume int
	persistDirty         bool

	exitErr error
	fatal   bool
}

const volumePersistDebounce = 600 * time.Millisecond

func Run(channelName, playlistURL string, settings config.Settings, manager *config.Manager) error {
	component := newApp(channelName, playlistURL, settings, manager)

	ui, err := gotui.NewApp(
		gotui.WithRootComponent(component),
	)
	if err != nil {
		return err
	}
	defer ui.Close()
	defer component.player.Stop()
	defer component.flushPersistVolume()

	if err := ui.Run(); err != nil {
		return err
	}

	return component.exitErr
}

func newApp(channelName, playlistURL string, settings config.Settings, manager *config.Manager) *app {
	settings = config.NormalizeSettings(settings)
	initialVolume := settings.Audio.Volume

	component := &app{
		channelName:   channelName,
		playlistURL:   playlistURL,
		channels:      config.Channels(),
		configManager: manager,

		mode:       gotui.NewState(viewBoot),
		status:     gotui.NewState("Checking dependencies"),
		errMessage: gotui.NewState(""),
		footerHint: gotui.NewState("Press q to quit"),
		categories: gotui.NewState([]radio.Category{}),
		selected:   gotui.NewState(0),
		bootEvent:  gotui.NewState(bootstrap.ProgressEvent{}),

		player:          radio.NewPlayer(initialVolume),
		currentCategory: gotui.NewState(radio.Category{}),
		volume:          gotui.NewState(initialVolume),
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
		updateChoice: gotui.NewState(0),
		bootCh:       make(chan bootstrap.ProgressEvent, 64),
		resultCh:     make(chan asyncResult, 8),
		vizHistory:   make([][4]float64, 0, 128),
	}

	component.startBootstrap()
	component.startCheckUpdate()
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
	a.updateChoice.BindApp(ui)
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

		if a.updateDone && a.updateRelease.TagName != "" {
			a.showUpdatePrompt(a.updateRelease)
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
		a.listOffset = 0
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

	case asyncCheckUpdate:
		a.updateDone = true
		if result.err != nil {
			a.updateError = result.err.Error()
			return
		}
		if !result.hasUpdate {
			return
		}
		a.updateRelease = result.release
		a.showUpdatePrompt(result.release)

	case asyncRunUpdate:
		a.updateDone = true
		if result.err != nil {
			a.setTransientError(fmt.Sprintf("update failed: %v", result.err))
			return
		}
		if !result.hasUpdate {
			a.status.Set("Already up to date")
		} else {
			a.status.Set(fmt.Sprintf("Updated to %s (%s)", result.release.TagName, result.installPath))
		}
		a.goToChannelSelector()
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

	// Update real spectrum history (4 bands: Bass, Low, Mid, High)
	if !paused && (a.player.Viz.HasData() || a.vizLiveHold > 0) {
		var entry [4]float64
		// Indices for 32 bands mapped logarithmically:
		// 0-4:   Sub-bass & Kicks
		// 5-12:  Low-mids & Snares
		// 13-22: Mids & Melody
		// 23-31: Highs & Percussion
		ranges := [][2]int{{0, 4}, {5, 12}, {13, 22}, {23, 31}}

		for i := 0; i < 4; i++ {
			sum := 0.0
			start, end := ranges[i][0], ranges[i][1]
			count := float64(end - start + 1)
			for j := start; j <= end; j++ {
				sum += a.vizBands[j]
			}
			entry[i] = sum / count

			// For BASS (index 0), we want it to be more punchy and less "always full"
			// by using a higher threshold or slightly more aggressive decay.
			if i == 0 {
				entry[i] = math.Pow(entry[i], 1.2) // increase contrast for bass
			}
		}
		a.vizHistory = append(a.vizHistory, entry)
		if len(a.vizHistory) > 128 {
			a.vizHistory = a.vizHistory[1:]
		}
	} else if len(a.vizHistory) > 0 {
		for i := range a.vizHistory {
			for j := 0; j < 4; j++ {
				a.vizHistory[i][j] *= 0.92
			}
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

	case viewUpdatePrompt:
		a.skipUpdatePrompt()
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

	case viewUpdatePrompt:
		choice := clamp(a.updateChoice.Get(), 0, 1)
		a.updateChoice.Set(choice)
		if choice == 0 {
			a.startSelfUpdate()
			return
		}
		a.skipUpdatePrompt()
		return

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
	if a.mode.Get() == viewUpdatePrompt {
		if delta != 0 {
			if a.updateChoice.Get() == 0 {
				a.updateChoice.Set(1)
			} else {
				a.updateChoice.Set(0)
			}
		}
		return
	}

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
	a.persistVolume(volume)
}

func (a *app) persistVolume(volume int) {
	if a.configManager == nil {
		return
	}

	a.persistMu.Lock()
	a.persistPendingVolume = volume
	a.persistDirty = true
	if a.persistTimer != nil {
		a.persistTimer.Stop()
	}
	a.persistTimer = time.AfterFunc(volumePersistDebounce, func() {
		a.flushPersistVolume()
	})
	a.persistMu.Unlock()
}

func (a *app) flushPersistVolume() {
	if a.configManager == nil {
		return
	}

	a.persistMu.Lock()
	if a.persistTimer != nil {
		a.persistTimer.Stop()
		a.persistTimer = nil
	}
	if !a.persistDirty {
		a.persistMu.Unlock()
		return
	}
	volume := a.persistPendingVolume
	a.persistDirty = false
	a.persistMu.Unlock()

	_, err := a.configManager.Update(func(settings *config.Settings) {
		settings.Audio.Volume = volume
	})
	if err != nil {
		radio.Logf("ui.config.save.error err=%v", err)
	}
}

func (a *app) goToSelector(status string) {
	if a.mode.Get() != viewPlayer && a.mode.Get() != viewSelect && a.mode.Get() != viewResolving && a.mode.Get() != viewError && a.mode.Get() != viewChannelSelect {
		return
	}

	if len(a.categories.Get()) == 0 {
		return
	}

	a.listOffset = 0
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
	a.listOffset = 0
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

func (a *app) startCheckUpdate() {
	if strings.EqualFold(strings.TrimSpace(version.Version), "dev") {
		a.updateDone = true
		return
	}

	go func() {
		release, hasUpdate, err := update.CheckLatest(version.Version)
		a.emitResult(asyncResult{
			kind:      asyncCheckUpdate,
			release:   release,
			hasUpdate: hasUpdate,
			err:       err,
		})
	}()
}

func (a *app) startSelfUpdate() {
	a.mode.Set(viewUpdating)
	a.status.Set("Updating to latest release")
	a.footerHint.Set("Please wait...")
	a.errMessage.Set("")
	go func() {
		release, updated, installPath, err := update.SelfUpdate(version.Version, "")
		a.emitResult(asyncResult{
			kind:        asyncRunUpdate,
			release:     release,
			hasUpdate:   updated,
			installPath: installPath,
			err:         err,
		})
	}()
}

func (a *app) showUpdatePrompt(release update.ReleaseInfo) {
	a.updateRelease = release
	a.updateChoice.Set(0)
	a.mode.Set(viewUpdatePrompt)
	a.status.Set("Update available")
	a.footerHint.Set("Up/Down select  Enter confirm  Esc skip")
}

func (a *app) skipUpdatePrompt() {
	a.updateRelease = update.ReleaseInfo{}
	a.goToChannelSelector()
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
		mainView = a.renderBoot(contentHeight)
	case viewChannelSelect:
		mainView = a.renderChannelSelector(termWidth, termHeight, contentHeight)
	case viewSelect:
		mainView = a.renderSelector(termWidth, termHeight, contentHeight)
	case viewResolving:
		mainView = a.renderResolving(contentHeight)
	case viewPlayer:
		mainView = a.renderPlayer(termWidth, termHeight, contentHeight)
	case viewUpdatePrompt:
		mainView = a.renderUpdatePrompt()
	case viewUpdating:
		mainView = a.renderUpdating()
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

func (a *app) renderBoot(contentHeight int) *gotui.Element {
	if strings.HasPrefix(a.status.Get(), "Connecting to") {
		return a.renderConnectingToChannel(contentHeight)
	}

	opts := []gotui.Option{
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 40, 100))),
		gotui.WithHeight(contentHeight),
		gotui.WithMinHeight(contentHeight),
		gotui.WithMaxHeight(contentHeight),
		gotui.WithFlexGrow(1),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithJustify(gotui.JustifyStart),
	}

	// Responsive padding & gaps
	var topPadding, bottomPadding int
	if contentHeight >= 14 {
		opts = append(opts, gotui.WithPaddingTRBL(2, 4, 2, 4), gotui.WithGap(0))
		topPadding = 2
		bottomPadding = 2
	} else if contentHeight >= 10 {
		opts = append(opts, gotui.WithPaddingTRBL(1, 2, 1, 2), gotui.WithGap(0))
		topPadding = 1
		bottomPadding = 1
	} else {
		opts = append(opts, gotui.WithPaddingTRBL(0, 1, 0, 1), gotui.WithGap(0))
		topPadding = 0
		bottomPadding = 0
	}

	// Calculate vertical heights to center perfectly inside
	childrenHeight := 0
	if contentHeight >= 12 {
		childrenHeight += 4 // ASCII art lines
		if contentHeight >= 14 {
			childrenHeight += 1 // ASCII spacer
		}
	} else {
		childrenHeight += 1 // Boot text line
	}

	childrenHeight += 1 // Spinner message line

	event := a.bootEvent.Get()
	if event.Type == bootstrap.ProgressEventDownload {
		childrenHeight += 1 // Progress bar
		if contentHeight >= 11 {
			childrenHeight += 1 // Progress detail line
		}
	}

	insideHeight := contentHeight - 2 - topPadding - bottomPadding
	topSpacer := 0
	if insideHeight > childrenHeight {
		topSpacer = (insideHeight - childrenHeight) / 2
	}

	box := gotui.New(opts...)

	// Add top spacer to center vertically
	if topSpacer > 0 {
		box.AddChild(gotui.New(gotui.WithHeight(topSpacer)))
	}

	// RENDER CONTENT
	if contentHeight >= 12 {
		asciiArt := []string{
			` _      ___  ___ ___`,
			`| |    / _ \| __|_ _|`,
			`| |__ | (_) | _| | | `,
			`|____| \___/|_| |___|`,
		}
		box.AddChild(renderASCIIBlock(asciiArt))
		if contentHeight >= 14 {
			box.AddChild(gotui.New(gotui.WithHeight(1)))
		}
	} else {
		box.AddChild(gotui.New(
			gotui.WithText("── LOFI RADIO BOOT ──"),
			gotui.WithTextStyle(gotui.NewStyle().Bold().Foreground(gotui.BrightWhite)),
			gotui.WithTextGradient(gotui.NewGradient(gotui.RGBColor(255, 40, 100), gotui.RGBColor(255, 200, 40)).WithDirection(gotui.GradientHorizontal)),
		))
	}

	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]

	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%s  %s", spin, sanitizeBootstrapMessage(a.status.Get()))),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan)),
	))

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
		if contentHeight >= 11 {
			box.AddChild(gotui.New(
				gotui.WithText(fmt.Sprintf("  %s / %s   %s", humanBytes(event.Download.BytesReceived), totalLabel, humanSpeed(event.Download.SpeedPerSec))),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
			))
		}
	}
	return box
}

func (a *app) renderConnectingToChannel(contentHeight int) *gotui.Element {
	pulse := a.pulsePhase.Get()
	borderColor := gotui.NewGradient(gotui.RGBColor(0, 220, 255), gotui.RGBColor(255, 0, 200)).At((math.Cos(pulse) + 1) / 2)

	opts := []gotui.Option{
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(borderColor)), // Glowing border
		gotui.WithHeight(contentHeight),
		gotui.WithMinHeight(contentHeight),
		gotui.WithMaxHeight(contentHeight),
		gotui.WithFlexGrow(1),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithJustify(gotui.JustifyStart),
	}

	// Responsive Padding & Gaps based on contentHeight
	var topPadding, bottomPadding int
	if contentHeight >= 17 {
		opts = append(opts, gotui.WithPaddingTRBL(2, 4, 2, 4), gotui.WithGap(0))
		topPadding = 2
		bottomPadding = 2
	} else if contentHeight >= 11 {
		opts = append(opts, gotui.WithPaddingTRBL(1, 2, 1, 2), gotui.WithGap(0))
		topPadding = 1
		bottomPadding = 1
	} else {
		opts = append(opts, gotui.WithPaddingTRBL(0, 1, 0, 1), gotui.WithGap(0))
		topPadding = 0
		bottomPadding = 0
	}

	// Calculate vertical heights to center perfectly inside
	childrenHeight := 0
	if contentHeight >= 16 {
		childrenHeight += 5 // ASCII block lines
		if contentHeight >= 17 {
			childrenHeight += 1 // Spacer height
		}
	} else {
		childrenHeight += 1 // Station Text line
	}

	if contentHeight >= 11 {
		childrenHeight += 1 // Station Detail line
	}

	childrenHeight += 1 // Station name line

	if contentHeight >= 15 {
		childrenHeight += 1 // Pre-status spacer height
	}

	childrenHeight += 1 // Status spinner line

	insideHeight := contentHeight - 2 - topPadding - bottomPadding
	topSpacer := 0
	if insideHeight > childrenHeight {
		topSpacer = (insideHeight - childrenHeight) / 2
	}

	box := gotui.New(opts...)

	// Add top spacer to center vertically
	if topSpacer > 0 {
		box.AddChild(gotui.New(gotui.WithHeight(topSpacer)))
	}

	tunerGradient := gotui.NewGradient(gotui.RGBColor(0, 255, 200), gotui.RGBColor(255, 0, 200)).WithDirection(gotui.GradientHorizontal)

	// 1. Beautiful ASCII text: TUNING
	if contentHeight >= 16 {
		box.AddChild(renderASCIIBlock([]string{
			` _____ _   _ _   _ ___ _   _  ____ `,
			`|_   _| | | | \ | |_ _| \ | |/ ___|`,
			`  | | | | | |  \| || ||  \| | |  _ `,
			`  | | | |_| | |\  || || |\  | |_| |`,
			`  |_|  \___/|_| \_|___|_| \_|\____|`,
		}))
		if contentHeight >= 17 {
			box.AddChild(gotui.New(gotui.WithHeight(1)))
		}
	} else {
		box.AddChild(gotui.New(
			gotui.WithText("── TUNING TO STATION ──"),
			gotui.WithTextStyle(gotui.NewStyle().Bold().Foreground(gotui.BrightWhite)),
			gotui.WithTextGradient(tunerGradient),
		))
	}

	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" %s ", strings.ToUpper(a.channelName))),
		gotui.WithTextStyle(gotui.NewStyle().Bold().Foreground(gotui.BrightWhite)),
		gotui.WithTextGradient(tunerGradient),
	))

	if contentHeight >= 15 {
		box.AddChild(gotui.New(gotui.WithHeight(1)))
	}

	// 4. Progress Telemetry
	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%s  LOADING CATEGORIES...", spin)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan).Bold()),
	))

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
		// Sticky scroll logic
		if selected < a.listOffset {
			a.listOffset = selected
		} else if selected >= a.listOffset+maxRows {
			a.listOffset = selected - maxRows + 1
		}

		start := a.listOffset
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
					dim = dim.Foreground(gotui.BrightGreen).Dim()
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
		// Sticky scroll logic
		if selected < a.listOffset {
			a.listOffset = selected
		} else if selected >= a.listOffset+maxRows {
			a.listOffset = selected - maxRows + 1
		}

		start := a.listOffset
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
					dim = dim.Foreground(gotui.BrightGreen).Dim()
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

func (a *app) renderResolving(contentHeight int) *gotui.Element {
	opts := []gotui.Option{
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 120, 0))), // Sunset orange border
		gotui.WithHeight(contentHeight),
		gotui.WithMinHeight(contentHeight),
		gotui.WithMaxHeight(contentHeight),
		gotui.WithFlexGrow(1),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithJustify(gotui.JustifyStart),
	}

	// Responsive padding & gaps
	var topPadding, bottomPadding int
	if contentHeight >= 17 {
		opts = append(opts, gotui.WithPaddingTRBL(2, 4, 2, 4), gotui.WithGap(0))
		topPadding = 2
		bottomPadding = 2
	} else if contentHeight >= 11 {
		opts = append(opts, gotui.WithPaddingTRBL(1, 2, 1, 2), gotui.WithGap(0))
		topPadding = 1
		bottomPadding = 1
	} else {
		opts = append(opts, gotui.WithPaddingTRBL(0, 1, 0, 1), gotui.WithGap(0))
		topPadding = 0
		bottomPadding = 0
	}

	// Calculate vertical heights to center perfectly inside
	childrenHeight := 0
	if contentHeight >= 16 {
		childrenHeight += 5 // ASCII block lines
		if contentHeight >= 17 {
			childrenHeight += 1 // Spacer height
		}
	} else {
		childrenHeight += 1 // STREAM text line
	}

	if contentHeight >= 11 {
		childrenHeight += 1 // Category Detail line
	}

	childrenHeight += 1 // Category name line

	if contentHeight >= 15 {
		childrenHeight += 1 // Pre-status spacer height
	}

	childrenHeight += 1 // Status spinner line

	insideHeight := contentHeight - 2 - topPadding - bottomPadding
	topSpacer := 0
	if insideHeight > childrenHeight {
		topSpacer = (insideHeight - childrenHeight) / 2
	}

	box := gotui.New(opts...)

	// Add top spacer to center vertically
	if topSpacer > 0 {
		box.AddChild(gotui.New(gotui.WithHeight(topSpacer)))
	}

	// Get resolving info
	categories := a.categories.Get()
	selected := a.selected.Get()
	categoryTitle := "Unknown Category"
	if selected >= 0 && selected < len(categories) {
		categoryTitle = compactText(categories[selected].Title, 64)
	}

	resolvingGradient := gotui.NewGradient(gotui.RGBColor(255, 140, 0), gotui.RGBColor(255, 230, 0)).WithDirection(gotui.GradientHorizontal)

	// 1. Beautiful ASCII text: STREAM
	if contentHeight >= 16 {
		box.AddChild(renderASCIIBlock([]string{
			`  ____ _____ ____  _____   _    __  __ `,
			` / ___|_   _|  _ \| ____| / \  |  \/  |`,
			` \___ \ | | | |_) |  _|  / _ \ | |\/| |`,
			`  ___)| | | |  _ <| |___/ ___ \| |  | |`,
			` |____/ |_| |_| \_\_____/_/  \_\_|  |_|`,
		}))
		if contentHeight >= 17 {
			box.AddChild(gotui.New(gotui.WithHeight(1)))
		}
	} else {
		box.AddChild(gotui.New(
			gotui.WithText("── CONNECTING TO STREAM ──"),
			gotui.WithTextStyle(gotui.NewStyle().Bold().Foreground(gotui.BrightWhite)),
			gotui.WithTextGradient(resolvingGradient),
		))
	}

	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" %s ── %s ", strings.ToUpper(a.channelName), strings.ToUpper(categoryTitle))),
		gotui.WithTextStyle(gotui.NewStyle().Bold().Foreground(gotui.BrightWhite)),
		gotui.WithTextGradient(resolvingGradient),
	))

	if contentHeight >= 15 {
		box.AddChild(gotui.New(gotui.WithHeight(1)))
	}

	// 4. Status
	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%s  CONNECTING TO SERVER...", spin)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightYellow).Bold()),
	))

	return box
}

func (a *app) renderUpdatePrompt() *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightYellow)),
		gotui.WithPaddingTRBL(1, 2, 1, 2),
		gotui.WithFlexGrow(1),
		gotui.WithGap(1),
	)

	box.AddChild(gotui.New(
		gotui.WithText(" UPDATE AVAILABLE"),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightYellow).Bold()),
	))
	box.AddChild(gotui.New(gotui.WithHR()))
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" Current: %s", version.Version)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
	))
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf(" Latest : %s", a.updateRelease.TagName)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
	))

	options := []string{"Update now", "Not now"}
	selected := clamp(a.updateChoice.Get(), 0, len(options)-1)
	for i := range options {
		prefix := "  "
		style := gotui.NewStyle().Foreground(gotui.BrightWhite)
		if i == selected {
			prefix = "▶ "
			style = gotui.NewStyle().Foreground(gotui.BrightCyan).Bold()
		}
		box.AddChild(gotui.New(
			gotui.WithText(prefix+options[i]),
			gotui.WithTextStyle(style),
		))
	}

	return box
}

func (a *app) renderUpdating() *gotui.Element {
	box := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.BrightCyan)),
		gotui.WithPaddingTRBL(1, 2, 1, 2),
		gotui.WithFlexGrow(1),
		gotui.WithGap(1),
		gotui.WithAlign(gotui.AlignCenter),
		gotui.WithJustify(gotui.JustifyCenter),
	)

	spin := spinnerBraille[a.spinnerFrame.Get()%len(spinnerBraille)]
	box.AddChild(gotui.New(
		gotui.WithText(fmt.Sprintf("%s  Installing %s ...", spin, a.updateRelease.TagName)),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan).Bold()),
	))
	box.AddChild(gotui.New(
		gotui.WithText(" Please wait, this may take a minute."),
		gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
	))
	return box
}

func (a *app) buildCassetteArt(isPaused bool) []string {
	spinners := []string{"◐", "◓", "◑", "◒"}
	var spinL, spinR string
	statusText := "  P L A Y I N G  "
	if isPaused {
		spinL = "o"
		spinR = "o"
		statusText = "  P A U S E D    "
	} else {
		frame := a.spinnerFrame.Get()
		spinL = spinners[frame%len(spinners)]
		spinR = spinners[(frame+2)%len(spinners)]
	}

	return []string{
		`  +-------------------------+`,
		fmt.Sprintf(`  | [%s]   (lofi-deck)   [%s] |`, spinL, spinR),
		`  |   +-----------------+   |`,
		fmt.Sprintf(`  |   |  %s  |   |`, statusText),
		`  |   +-----------------+   |`,
		`  +-------------------------+`,
	}
}

func (a *app) buildCompactCassetteArt(isPaused bool) []string {
	spinners := []string{"◐", "◓", "◑", "◒"}
	var spinL, spinR string
	if isPaused {
		spinL = "o"
		spinR = "o"
	} else {
		frame := a.spinnerFrame.Get()
		spinL = spinners[frame%len(spinners)]
		spinR = spinners[(frame+2)%len(spinners)]
	}

	return []string{
		`  +-------------------------+`,
		fmt.Sprintf(`  | [%s]   (lofi-deck)   [%s] |`, spinL, spinR),
		`  +-------------------------+`,
	}
}

func (a *app) renderPlayer(termWidth, termHeight, contentHeight int) *gotui.Element {
	// Root row container
	row := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
		gotui.WithFlexGrow(1),
		gotui.WithGap(1),
	)

	category := a.currentCategory.Get()
	isPaused := a.paused.Get()

	// LEFT COLUMN (Info & Controls - 38% width)
	leftCol := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithWidth(int(float64(termWidth)*0.38)),
		gotui.WithMinWidth(42),
		gotui.WithFlexShrink(0),
		gotui.WithHeight(contentHeight),
		gotui.WithMinHeight(contentHeight),
		gotui.WithMaxHeight(contentHeight),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 0, 150))), // Synthwave Pink
		gotui.WithPaddingTRBL(1, 2, 1, 2),
		gotui.WithGap(0),
	)

	// Consistent Indentation
	indent := gotui.WithPaddingTRBL(0, 1, 0, 0)

	maxTitleLen := 32
	if termWidth > 130 {
		maxTitleLen = 42
	}

	vol := a.volume.Get()
	speakerLabel := "VOL"
	if vol == 0 {
		speakerLabel = "MUTE"
	}

	playbackStats := a.player.Stats()
	sigIdx, bitrateText := signalAndBitrate(playbackStats)
	sigBars := []string{" ", "▂", "▃", "▅", "▆", "█"}

	// RESPONSIVE LAYOUT DISPATCHER
	if contentHeight >= 20 {
		// 1. Station Section
		leftCol.AddChild(gotui.New(
			gotui.WithText(" [STATION]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))
		leftCol.AddChild(gotui.New(
			gotui.WithText(" > "+pingPongScrollText(category.Title, maxTitleLen, a.aniTick)),
			gotui.WithTextGradient(gotui.NewGradient(gotui.Yellow, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
			gotui.WithTextStyle(gotui.NewStyle().Bold()),
			indent,
		))
		leftCol.AddChild(gotui.New(
			gotui.WithText("   "+strings.ToUpper(a.playingChannelName)),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan).Bold().Dim()),
			indent,
		))

		// 2. Cassette Deck
		leftCol.AddChild(gotui.New(gotui.WithHR()))
		leftCol.AddChild(gotui.New(
			gotui.WithText(" [CASSETTE DECK]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))

		cassetteLines := a.buildCassetteArt(isPaused)
		cassetteBox := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
			gotui.WithGap(0),
			gotui.WithPaddingTRBL(0, 1, 0, 0),
		)

		cassetteGradient := gotui.NewGradient(gotui.RGBColor(255, 0, 150), gotui.RGBColor(0, 200, 255)).WithDirection(gotui.GradientHorizontal)
		for _, line := range cassetteLines {
			cassetteBox.AddChild(gotui.New(
				gotui.WithText(line),
				gotui.WithWrap(false),
				gotui.WithTextGradient(cassetteGradient),
				gotui.WithTextStyle(gotui.NewStyle().Bold()),
			))
		}
		leftCol.AddChild(cassetteBox)

		// 3. Playback Section
		leftCol.AddChild(gotui.New(gotui.WithHR()))
		leftCol.AddChild(gotui.New(
			gotui.WithText(" [PLAYBACK STATS]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))

		infoRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(2),
			gotui.WithAlign(gotui.AlignCenter),
			indent,
		)

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
		sigRow.AddChild(gotui.New(
			gotui.WithText("SIGNAL: "+sigBars[sigIdx]),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
		sigRow.AddChild(gotui.New(
			gotui.WithText(bitrateText),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		))
		leftCol.AddChild(sigRow)

		// Engine info line
		metaRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(1),
			indent,
		)
		metaRow.AddChild(gotui.New(
			gotui.WithText("ENGINE: MP3/AAC DIRECT DECODE"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim()),
		))
		leftCol.AddChild(metaRow)

		// 4. Audio Section
		leftCol.AddChild(gotui.New(gotui.WithHR()))
		leftCol.AddChild(gotui.New(
			gotui.WithText(" [AUDIO VOLUME]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))

		volRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(1),
			gotui.WithAlign(gotui.AlignCenter),
			indent,
		)

		volGradient := gotui.NewGradient(gotui.RGBColor(0, 255, 200), gotui.RGBColor(255, 0, 150)).WithDirection(gotui.GradientHorizontal)
		volRow.AddChild(gotui.New(
			gotui.WithText(speakerLabel+" "+renderFancyBar(int64(vol), 100, 16)),
			gotui.WithTextGradient(volGradient),
			gotui.WithTextStyle(gotui.NewStyle().Bold()),
		))
		volRow.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("%d%%", vol)),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
		))
		leftCol.AddChild(volRow)

	} else if contentHeight >= 14 {
		// Medium Height layout
		leftCol.AddChild(gotui.New(
			gotui.WithText(" [STATION]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))
		leftCol.AddChild(gotui.New(
			gotui.WithText("> "+pingPongScrollText(category.Title, maxTitleLen, a.aniTick)),
			gotui.WithTextGradient(gotui.NewGradient(gotui.Yellow, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
			gotui.WithTextStyle(gotui.NewStyle().Bold()),
			indent,
		))

		leftCol.AddChild(gotui.New(gotui.WithHR()))

		cassetteLines := a.buildCompactCassetteArt(isPaused)
		cassetteBox := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
			gotui.WithGap(0),
			gotui.WithPaddingTRBL(0, 1, 0, 0),
		)
		cassetteGradient := gotui.NewGradient(gotui.RGBColor(255, 0, 150), gotui.RGBColor(0, 200, 255)).WithDirection(gotui.GradientHorizontal)
		for _, line := range cassetteLines {
			cassetteBox.AddChild(gotui.New(
				gotui.WithText(line),
				gotui.WithWrap(false),
				gotui.WithTextGradient(cassetteGradient),
				gotui.WithTextStyle(gotui.NewStyle().Bold()),
			))
		}
		leftCol.AddChild(cassetteBox)

		leftCol.AddChild(gotui.New(gotui.WithHR()))

		statusText := "PLAYING"
		statusStyle := gotui.NewStyle().Foreground(gotui.BrightGreen)
		if isPaused {
			statusText = "PAUSED "
			statusStyle = gotui.NewStyle().Foreground(gotui.BrightYellow)
		}

		// Combined Stats & Volume
		leftCol.AddChild(gotui.New(
			gotui.WithText(" [PLAYBACK & VOLUME]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))

		statRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(1),
			indent,
		)
		statRow.AddChild(gotui.New(
			gotui.WithText(statusText+" "+a.playbackElapsed()+" | "+speakerLabel),
			gotui.WithTextStyle(statusStyle.Bold()),
		))
		statRow.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf(" %d%%", vol)),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightWhite).Bold()),
		))
		leftCol.AddChild(statRow)

		sigRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(1),
			indent,
		)
		sigRow.AddChild(gotui.New(
			gotui.WithText("SIGNAL: "+sigBars[sigIdx]+" | "+bitrateText),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
		leftCol.AddChild(sigRow)

	} else {
		// Low height layout (ultra compact)
		leftCol := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
			gotui.WithWidth(int(float64(termWidth)*0.38)),
			gotui.WithMinWidth(42),
			gotui.WithFlexShrink(0),
			gotui.WithHeight(contentHeight),
			gotui.WithMinHeight(contentHeight),
			gotui.WithMaxHeight(contentHeight),
			gotui.WithBorder(gotui.BorderRounded),
			gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 0, 150))), // Synthwave Pink
			gotui.WithPaddingTRBL(0, 2, 0, 2),
			gotui.WithGap(0),
		)

		leftCol.AddChild(gotui.New(
			gotui.WithText("> "+pingPongScrollText(category.Title, maxTitleLen-4, a.aniTick)),
			gotui.WithTextGradient(gotui.NewGradient(gotui.Yellow, gotui.BrightWhite).WithDirection(gotui.GradientHorizontal)),
			gotui.WithTextStyle(gotui.NewStyle().Bold()),
		))

		statusText := "PLAYING"
		statusStyle := gotui.NewStyle().Foreground(gotui.BrightGreen)
		if isPaused {
			statusText = "PAUSED "
			statusStyle = gotui.NewStyle().Foreground(gotui.BrightYellow)
		}

		statRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(1),
		)
		statRow.AddChild(gotui.New(
			gotui.WithText(statusText+" "+a.playbackElapsed()+" | "+speakerLabel+fmt.Sprintf(" %d%%", vol)),
			gotui.WithTextStyle(statusStyle.Bold()),
		))
		leftCol.AddChild(statRow)

		sigRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(1),
		)
		sigRow.AddChild(gotui.New(
			gotui.WithText("SIGNAL: "+sigBars[sigIdx]+" | ENGINE: MP3/AAC"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack)),
		))
		leftCol.AddChild(sigRow)
	}

	// RIGHT COLUMN (Visuals - 62% width)
	rightCol := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithFlexGrow(1),
		gotui.WithHeight(contentHeight),
		gotui.WithMinHeight(contentHeight),
		gotui.WithMaxHeight(contentHeight),
		gotui.WithGap(0),
	)

	// Responsive heights for right column boxes
	vizHeight := 7
	vizRows := 4
	if termWidth >= 145 {
		vizHeight = 8
		vizRows = 5
	}

	if contentHeight < 14 {
		// Adjust for tiny terminal height
		vizHeight = 5
		vizRows = 3
	}

	// Check if we should render both Waterfall and Wave Visualizer
	if contentHeight >= 11 {
		// Top: Background Decor (Waterfall)
		decorBox := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
			gotui.WithFlexGrow(1),
			gotui.WithBorder(gotui.BorderRounded),
			gotui.WithBorderStyle(gotui.NewStyle().Foreground(gotui.RGBColor(0, 180, 255))), // Cyber Teal
			gotui.WithPaddingTRBL(0, 2, 0, 2),
		)

		decorBox.AddChild(gotui.New(
			gotui.WithText(" [SPECTRUM WATERFALL]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Bold().Dim()),
		))

		// Create a waterfall plot with 4 frequency rows (responsive)
		waterfall := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
			gotui.WithFlexGrow(1),
			gotui.WithJustify(gotui.JustifyCenter),
			gotui.WithGap(0),
		)

		histChars := []string{" ", " ", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
		maxHistLen := (termWidth - 42) - 10

		bandLabels := []string{" HI ", " MID", " LOW", " BASS"}
		bandGradients := []gotui.Gradient{
			gotui.NewGradient(gotui.RGBColor(200, 100, 255), gotui.RGBColor(255, 100, 200)),
			gotui.NewGradient(gotui.RGBColor(100, 150, 255), gotui.RGBColor(150, 100, 255)),
			gotui.NewGradient(gotui.RGBColor(50, 200, 200), gotui.RGBColor(100, 200, 255)),
			gotui.NewGradient(gotui.RGBColor(50, 255, 150), gotui.RGBColor(50, 200, 200)),
		}

		// Adjust rows if height is tight
		waterfallRows := 4
		if contentHeight < 15 {
			waterfallRows = 3
		}

		for i := 0; i < waterfallRows; i++ {
			row := gotui.New(
				gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
				gotui.WithGap(1),
			)

			row.AddChild(gotui.New(
				gotui.WithText(bandLabels[i]),
				gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightBlack).Dim().Bold()),
			))

			if maxHistLen > 8 {
				history := a.vizHistory
				histWidth := maxHistLen - 6
				if len(history) > histWidth {
					history = history[len(history)-histWidth:]
				}

				graphStr := ""
				for _, entry := range history {
					val := entry[3-i]
					val *= 1.4
					idx := int(val * float64(len(histChars)-1))
					if idx < 0 {
						idx = 0
					}
					if idx >= len(histChars) {
						idx = len(histChars) - 1
					}
					graphStr += histChars[idx]
				}
				row.AddChild(gotui.New(
					gotui.WithText(graphStr),
					gotui.WithTextGradient(bandGradients[i]),
				))
			}
			waterfall.AddChild(row)
		}

		// Add technical labels to the bottom of decor box
		statsRow := gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Row),
			gotui.WithGap(3),
		)

		peak := 0.0
		for _, entry := range a.vizHistory {
			for _, v := range entry {
				if v > peak {
					peak = v
				}
			}
		}

		statsRow.AddChild(gotui.New(
			gotui.WithText(fmt.Sprintf("[PEAK: %.2f]", peak)),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.RGBColor(255, 0, 128)).Bold()),
		))
		statsRow.AddChild(gotui.New(
			gotui.WithText("[DSP: 32-BAND]"),
			gotui.WithTextStyle(gotui.NewStyle().Foreground(gotui.BrightCyan).Bold()),
		))

		decorBox.AddChild(waterfall)
		decorBox.AddChild(statsRow)
		rightCol.AddChild(decorBox)
	}

	// Bottom: Wave Visualizer Box
	pulse := a.pulsePhase.Get()
	vizBorderColor := gotui.NewGradient(gotui.RGBColor(0, 200, 255), gotui.RGBColor(255, 0, 200)).At((math.Cos(pulse) + 1) / 2)
	visualizerBox := gotui.New(
		gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
		gotui.WithHeight(vizHeight),
		gotui.WithMinHeight(vizHeight),
		gotui.WithMaxHeight(vizHeight),
		gotui.WithBorder(gotui.BorderRounded),
		gotui.WithBorderStyle(gotui.NewStyle().Foreground(vizBorderColor)),
		gotui.WithPaddingTRBL(0, 1, 0, 1),
	)

	// If no waterfall is rendered, let the wave visualizer grow to fill the space
	if contentHeight < 11 {
		visualizerBox = gotui.New(
			gotui.WithDisplay(gotui.DisplayFlex), gotui.WithDirection(gotui.Column),
			gotui.WithFlexGrow(1),
			gotui.WithBorder(gotui.BorderRounded),
			gotui.WithBorderStyle(gotui.NewStyle().Foreground(vizBorderColor)),
			gotui.WithPaddingTRBL(0, 1, 0, 1),
		)
		vizRows = contentHeight - 2
		if vizRows < 3 {
			vizRows = 3
		}
	}

	visualizerBox.AddChild(a.buildWaveVisualizer(isPaused, termWidth, vizRows))
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
	case viewUpdatePrompt:
		return "Update Available"
	case viewUpdating:
		return "Updating"
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

func signalAndBitrate(stats radio.PlaybackStats) (int, string) {
	if !stats.Running {
		return 1, "0KBPS"
	}

	bitrateText := fmt.Sprintf("%.0fKBPS", stats.OutputKbps)
	if stats.OutputKbps < 1 {
		bitrateText = "0KBPS"
	}

	// PCM output target: 48kHz * stereo * 16-bit ~= 1536 kbps.
	base := clamp(int(math.Round((stats.OutputKbps/1536.0)*5.0)), 1, 5)
	if !stats.AnalyzerFresh {
		base -= 1
	}
	if !stats.LastReconnect.IsZero() && time.Since(stats.LastReconnect) <= 8*time.Second {
		base -= 1
	}
	base = clamp(base, 1, 5)
	return base, bitrateText
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

func pingPongScrollText(value string, maxLen int, tick int) string {
	clean := strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	runes := []rune(clean)
	L := len(runes)
	if maxLen <= 0 || L <= maxLen {
		return clean
	}

	M := L - maxLen
	const waitTicks = 60
	const ticksPerChar = 6

	cycleLength := 2*waitTicks + 2*M*ticksPerChar
	t := tick % cycleLength

	if t < waitTicks {
		return string(runes[:maxLen])
	}
	t -= waitTicks

	if t < M*ticksPerChar {
		idx := t / ticksPerChar
		return string(runes[idx : idx+maxLen])
	}
	t -= M * ticksPerChar

	if t < waitTicks {
		return string(runes[M : M+maxLen])
	}
	t -= waitTicks

	idx := max(M-(t/ticksPerChar), 0)
	return string(runes[idx : idx+maxLen])
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
