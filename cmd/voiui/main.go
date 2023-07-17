package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget/material"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/getlantern/systray"
	"github.com/pkg/errors"
)

//go:embed voi.ico
var voiIcon []byte

type state struct {
	running bool

	round         uint64
	participating bool
	progress      float32

	prevBlockDuration time.Duration
	currBlockAt       time.Time
}

type updateCb func(*state) error

type program struct {
	url   string
	token string

	ac *algod.Client

	updates chan updateCb

	s state
}

func (p *program) runFrontend(ctx context.Context, w *app.Window) error {
	th := material.NewTheme(gofont.Collection())

	t := time.NewTicker(time.Millisecond * 20)
	defer t.Stop()

	var ops op.Ops
	for {
		select {
		case <-t.C:
			if p.s.prevBlockDuration != 0 {
				diff := time.Since(p.s.currBlockAt)
				p.s.progress = 1 - float32(diff)/float32(p.s.prevBlockDuration)
			}
			w.Invalidate()
		case <-ctx.Done():
			log.Println("context done")
			return ctx.Err()
		case e := <-p.updates:
			err := e(&p.s)
			if err != nil {
				return errors.Wrap(err, "failed to update state")
			}
			w.Invalidate()
		case e := <-w.Events():
			switch e := e.(type) {
			case system.DestroyEvent:
				return e.Err
			case system.FrameEvent:
				type (
					C = layout.Context
					D = layout.Dimensions
				)

				gtx := layout.NewContext(&ops, e)

				layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						in := layout.UniformInset(unit.Dp(8))
						return in.Layout(gtx, func(gtx C) D {
							return layout.Flex{Axis: layout.Vertical}.Layout(
								gtx,
								layout.Rigid(func(gtx C) D {
									title := material.Caption(th, "Address:")
									return title.Layout(gtx)
								}),
								layout.Rigid(func(gtx C) D {
									running := material.Body1(th, p.url)
									return running.Layout(gtx)
								}),
							)
						})
					}),
					layout.Rigid(func(gtx C) D {
						in := layout.UniformInset(unit.Dp(8))

						var text string
						if p.s.running {
							text = "Running"
						} else {
							text = "Not Running"
						}

						title := material.Subtitle1(th, text)
						if p.s.running {
							title.Color = color.NRGBA{R: 0x00, G: 0xaa, B: 0x00, A: 0xff}
						} else {
							title.Color = color.NRGBA{R: 0xaa, G: 0x00, B: 0x00, A: 0xff}
						}

						return in.Layout(gtx, func(gtx C) D { return title.Layout(gtx) })
					}),
					layout.Rigid(func(gtx C) D {
						in := layout.UniformInset(unit.Dp(8))
						return in.Layout(gtx, func(gtx C) D {
							return layout.Flex{Axis: layout.Vertical}.Layout(
								gtx,
								layout.Rigid(func(gtx C) D {
									title := material.Caption(th, "Last round:")
									return title.Layout(gtx)
								}),
								layout.Rigid(func(gtx C) D {
									running := material.Body1(th, fmt.Sprintf("%d", p.s.round))
									return running.Layout(gtx)
								}),
							)
						})
					}),
					layout.Rigid(func(gtx C) D {
						in := layout.UniformInset(unit.Dp(8))

						var text string
						if p.s.participating {
							text = "Participating"
						} else {
							text = "Not participating"
						}

						title := material.Subtitle1(th, text)
						if p.s.participating {
							title.Color = color.NRGBA{R: 0x00, G: 0xaa, B: 0x00, A: 0xff}
						} else {
							title.Color = color.NRGBA{R: 0xaa, G: 0x00, B: 0x00, A: 0xff}
						}

						return in.Layout(gtx, func(gtx C) D { return title.Layout(gtx) })
					}),
					layout.Rigid(func(gtx C) D {
						bar := material.ProgressBar(th, p.s.progress)
						return bar.Layout(gtx)
					}),
				)

				e.Frame(gtx.Ops)
			}
		}
	}
}

type Participation struct {
	Address             string  `json:"address"`
	EffectiveFirstValid *uint64 `json:"effective-first-valid"`
	EffectiveLastValid  *uint64 `json:"effective-last-valid"`
	Id                  string  `json:"id"`
}

func (p *program) runBackend() error {
	status, err := p.ac.Status().Do(context.Background())
	if err != nil {
		return errors.Wrap(err, "failed to get status")
	}

	round := status.LastRound

	p.updates <- func(s *state) error {
		s.round = round
		s.running = true
		return nil
	}

	for {
		status, err = p.ac.StatusAfterBlock(status.LastRound).Do(context.Background())
		if err != nil {
			p.updates <- func(s *state) error {
				s.running = false
				return nil
			}
			return errors.Wrap(err, "failed to get status")
		}

		round := status.LastRound
		currBlockAt := time.Now()

		p.updates <- func(s *state) error {
			s.round = round
			s.running = true

			s.prevBlockDuration = currBlockAt.Sub(s.currBlockAt)
			s.currBlockAt = currBlockAt
			return nil
		}

		err = func() error {
			req, err := http.NewRequest("GET", fmt.Sprintf("%s/v2/participation", p.url), nil)
			if err != nil {
				return errors.Wrap(err, "failed to create participation request")
			}

			req.Header.Set("X-Algo-API-Token", p.token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return errors.Wrap(err, "failed to do participation request")
			}

			defer resp.Body.Close()

			if resp.StatusCode >= 400 {
				return errors.Errorf("failed to check participation: %s", resp.Status)
			}

			var items []Participation

			err = json.NewDecoder(resp.Body).Decode(&items)
			if err != nil {
				return errors.Wrap(err, "failed to decode participation response")
			}

			participating := false

			for _, item := range items {
				if item.EffectiveFirstValid != nil && *item.EffectiveFirstValid >= status.LastRound && item.EffectiveLastValid != nil && *item.EffectiveLastValid <= status.LastRound {
					participating = true
					break
				}
			}

			p.updates <- func(s *state) error {
				s.participating = participating
				return nil
			}

			return nil
		}()

		if err != nil {
			return err
		}
	}
}

func run(a args) error {
	if a.Path != "" && (a.Algod != "" || a.Token != "") {
		return errors.New("cannot specify -path with -algod or -token")
	}

	var url string
	var token string

	if a.Algod != "" {
		url = a.Algod
		token = a.Token
	} else {
		if a.Path == "" {
			a.Path = "data"
		}

		addrBytes, err := os.ReadFile(filepath.Join(a.Path, "algod.net"))
		if err != nil {
			return errors.Wrap(err, "failed to read algod.net")
		}

		addr := strings.TrimSpace(string(addrBytes))

		tokenBytes, err := os.ReadFile(filepath.Join(a.Path, "algod.admin.token"))
		if err != nil {
			return errors.Wrap(err, "failed to read algod.admin.token")
		}

		token = strings.TrimSpace(string(tokenBytes))
		url = fmt.Sprintf("http://%s", addr)
	}

	ac, err := algod.MakeClient(url, token)
	if err != nil {
		return errors.Wrap(err, "failed to make algod client")
	}

	updates := make(chan updateCb)

	ctx, cancel := context.WithCancel(context.Background())

	p := &program{
		url:     url,
		token:   token,
		ac:      ac,
		updates: updates,
		s: state{
			progress: 1.0,
		},
	}

	runWindow := func() {
		w := app.NewWindow()
		w.Option(
			app.Title("Voi Node Monitor"),
			app.Size(unit.Dp(300), unit.Dp(200)),
			app.MinSize(unit.Dp(300), unit.Dp(200)),
		)

		err := p.runFrontend(ctx, w)
		fmt.Println("run exited", err)
		if err != nil {
			log.Fatal(err)
		}
	}

	go func() {
		for {
			err := p.runBackend()
			if err != nil {
				log.Printf("error: %v", err)
			}
		}
	}()

	systray.Run(func() {
		// TODO: set icon
		systray.SetIcon(voiIcon)
		systray.SetTitle("Voi Node Monitor")

		mOpen := systray.AddMenuItem("Open", "Open monitor")
		mQuit := systray.AddMenuItem("Quit", "Quit monitor")

		go func() {
			runWindow()

		loop:
			for {
				select {
				case <-mOpen.ClickedCh:
					runWindow()
				case <-ctx.Done():
					break loop
				}
			}

			fmt.Println("open done")
		}()

		go func() {
			<-mQuit.ClickedCh
			// TODO: Quit probably must be called for alt+f4 too
			systray.Quit()
			cancel()

			fmt.Println("quit done")

			os.Exit(0)
		}()

	}, nil)

	app.Main()

	fmt.Println("main done")

	return nil
}

type args struct {
	Path string

	Algod string
	Token string
}

func main() {
	var a args

	flag.StringVar(&a.Path, "path", "", "path to node data")
	// or
	flag.StringVar(&a.Algod, "algod", "", "algod address")
	flag.StringVar(&a.Token, "token", "", "algod admin token")

	flag.Parse()

	err := run(a)
	if err != nil {
		panic(err)
	}
}
