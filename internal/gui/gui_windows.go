package gui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/walk"
	. "github.com/tailscale/walk/declarative"
	"golang.org/x/sys/windows"

	"mpquic/internal/config"
	"mpquic/internal/logfile"
	"mpquic/internal/winsvc"
)

// Options configure the configurator.
type Options struct {
	Name    string // service instance
	Version string
	// SmokeTest > 0 opens the window without elevation, exercises the form
	// and closes it after this long (used by CI).
	SmokeTest time.Duration
	// Elevated is set on the instance started through the UAC prompt, so
	// that it never relaunches itself again.
	Elevated bool
}

var roleLabels = []string{
	"Local: SOCKS5 proxy (like ss-local)",
	"Tunnel: fixed-target port forward (like ss-tunnel)",
	"Remote: exit server (like ss-server)",
	"Relay: UDP forwarder (like socat)",
}

var (
	congestionValues = []string{"", "bbr-aggressive", "bbr-conservative", "cubic"}
	congestionLabels = []string{"bbr (default)", "bbr-aggressive", "bbr-conservative", "cubic (quic-go built-in)"}
	logLevels        = []string{"info", "debug", "warn", "error"}
)

const logTab = 2

type ui struct {
	opt  Options
	mu   sync.Mutex
	name string // guarded by mu (read by the status poller)

	base   *config.Config // the loaded config (keys the form does not edit are kept)
	loaded Form           // form state as loaded, for change detection
	busy   bool
	status winsvc.Status

	mw                                     *walk.MainWindow
	nameCB, roleCB, congCB, logCB          *walk.ComboBox
	reverseCB, udpCB, noDupCB, showPwCB    *walk.CheckBox
	raceCB                                 *walk.CheckBox
	statusLbl, hintLbl, pathsLbl, portLbl  *walk.Label
	laddrLbl, lportLbl, tunnelLbl          *walk.Label
	relaysLbl, relaysHelp                  *walk.Label
	quicRows                               *walk.Composite
	quicGB, outboundGB                     *walk.GroupBox
	pathsTE, relaysTE, logTE               *walk.TextEdit
	portLE, pwLE, laddrLE, lportLE         *walk.LineEdit
	tunnelLE, windowLE, chunkLE, timeoutLE *walk.LineEdit
	mtuLE                                  *walk.LineEdit
	statsLE, certLE, keyLE, sniLE          *walk.LineEdit
	tabs                                   *walk.TabWidget
	applyPB, startPB, stopPB, restartPB    *walk.PushButton
	uninstallPB                            *walk.PushButton
	sbItem                                 *walk.StatusBarItem
	lastLog                                string
}

// Run shows the configurator. Without administrator rights it relaunches
// itself through the UAC prompt.
func Run(opt Options) error {
	hideOwnConsole()
	if opt.Name == "" {
		opt.Name = winsvc.DefaultName
	}
	if opt.SmokeTest == 0 && !opt.Elevated && !winsvc.IsElevated() {
		err := relaunchElevated([]string{"gui", "--elevated", "--name", opt.Name})
		if errors.Is(err, windows.ERROR_CANCELLED) {
			messageBox("MPQUIC", "Administrator rights are required to configure the MPQUIC service.", windows.MB_ICONWARNING)
			return nil
		}
		return err
	}
	wapp, err := walk.InitApp()
	if err != nil {
		return err
	}
	u := &ui{opt: opt, name: opt.Name}
	if err := u.create(); err != nil {
		messageBox("MPQUIC", "Cannot create the window: "+err.Error(), windows.MB_ICONERROR)
		return err
	}
	// The main window always has a toolbar; ours is empty. Hide it explicitly
	// (Wine otherwise draws it over the first row).
	u.mw.ToolBar().SetVisible(false)
	if icon, err := walk.NewIconFromResource("APP"); err == nil {
		u.mw.SetIcon(icon)
	}
	u.load(opt.Name)
	u.mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if u.dirty() && walk.MsgBox(u.mw, "MPQUIC", "Discard the unsaved changes?",
			walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
			*canceled = true
		}
	})
	stop := make(chan struct{})
	go u.poll(stop)
	var smokeErr error
	if opt.SmokeTest > 0 {
		time.AfterFunc(opt.SmokeTest, func() {
			u.mw.Synchronize(func() {
				smokeErr = u.smokeTest()
				u.loaded = u.readForm() // skip the unsaved-changes prompt
				u.mw.Close()
			})
		})
	}
	wapp.Run()
	close(stop)
	if opt.SmokeTest > 0 {
		if smokeErr != nil {
			return fmt.Errorf("gui smoke test: %w", smokeErr)
		}
		fmt.Println("gui smoke test ok")
	}
	return nil
}

func (u *ui) create() error {
	instances := winsvc.Instances()
	// Fixed-width labels keep the separate grids of a page aligned.
	label := func(text string, assign **walk.Label) Label {
		return Label{Text: text, AssignTo: assign, TextAlignment: AlignFar, MinSize: Size{Width: 110}}
	}
	browse := func(le **walk.LineEdit, title string) PushButton {
		return PushButton{Text: "Browse...", OnClicked: func() {
			dlg := walk.FileDialog{Title: title, Filter: "PEM files (*.pem;*.crt;*.key)|*.pem;*.crt;*.key|All files (*.*)|*.*"}
			if ok, _ := dlg.ShowOpen(u.mw); ok {
				(*le).SetText(dlg.FilePath)
			}
		}}
	}
	title := "MPQUIC Configurator"
	if u.opt.Version != "" && u.opt.Version != "dev" {
		title += " " + u.opt.Version
	}
	return MainWindow{
		AssignTo: &u.mw,
		Title:    title,
		MinSize:  Size{Width: 620, Height: 560},
		Size:     Size{Width: 760, Height: 700},
		Layout:   VBox{},
		MenuItems: []MenuItem{
			Menu{Text: "&File", Items: []MenuItem{
				Action{Text: "&Import config...", OnTriggered: u.importConfig},
				Action{Text: "&Export config...", OnTriggered: u.exportConfig},
				Separator{},
				Action{Text: "Open &data folder", OnTriggered: func() { openFolder(winsvc.DataDir()) }},
				Separator{},
				Action{Text: "E&xit", OnTriggered: func() { u.mw.Close() }},
			}},
			Menu{Text: "&Help", Items: []MenuItem{
				Action{Text: "&About", OnTriggered: u.about},
			}},
		},
		StatusBarItems: []StatusBarItem{{AssignTo: &u.sbItem, Width: 600}},
		Children: []Widget{
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				Label{Text: "Service:"},
				ComboBox{AssignTo: &u.nameCB, Editable: true, Model: instances, MinSize: Size{Width: 160},
					OnCurrentIndexChanged: func() {
						if i := u.nameCB.CurrentIndex(); i >= 0 && i < len(instances) {
							u.switchTo(instances[i])
						}
					},
					OnEditingFinished: func() { u.switchTo(strings.TrimSpace(u.nameCB.Text())) },
				},
				HSpacer{Size: 12},
				Label{Text: "Status:"},
				Label{AssignTo: &u.statusLbl, Text: "...", Font: Font{Family: "Segoe UI", PointSize: 9, Bold: true}},
				HSpacer{},
			}},
			TabWidget{AssignTo: &u.tabs, OnCurrentIndexChanged: u.refreshLog, Pages: []TabPage{
				{Title: "Connection", Layout: VBox{}, Children: []Widget{
					Composite{Layout: Grid{Columns: 2, MarginsZero: true}, Children: []Widget{
						label("Role:", nil),
						ComboBox{AssignTo: &u.roleCB, Model: roleLabels, OnCurrentIndexChanged: u.updateRole},
						label("", nil),
						CheckBox{AssignTo: &u.reverseCB, Text: "Reverse mode: the local side has the public address (set on both ends)",
							OnCheckedChanged: u.updateRole},
					}},
					Label{AssignTo: &u.pathsLbl},
					TextEdit{AssignTo: &u.pathsTE, VScroll: true, MinSize: Size{Height: 90}},
					Composite{AssignTo: &u.quicRows, Layout: Grid{Columns: 2, MarginsZero: true}, Children: []Widget{
						label("Port:", &u.portLbl),
						LineEdit{AssignTo: &u.portLE, CueBanner: "8388"},
						label("Password:", nil),
						Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
							LineEdit{AssignTo: &u.pwLE, PasswordMode: true},
							CheckBox{AssignTo: &u.showPwCB, Text: "Show", OnCheckedChanged: func() { u.pwLE.SetPasswordMode(!u.showPwCB.Checked()) }},
							PushButton{Text: "Generate", OnClicked: u.generatePassword},
						}},
						label("", nil),
						CheckBox{AssignTo: &u.udpCB, Text: "Relay UDP (SOCKS5 UDP ASSOCIATE; enable on both ends)"},
						label("Listen address:", &u.laddrLbl),
						LineEdit{AssignTo: &u.laddrLE, CueBanner: "127.0.0.1"},
						label("Listen port:", &u.lportLbl),
						LineEdit{AssignTo: &u.lportLE, CueBanner: "1080"},
						label("Tunnel target:", &u.tunnelLbl),
						LineEdit{AssignTo: &u.tunnelLE, CueBanner: "host:port"},
					}},
					Label{AssignTo: &u.relaysLbl, Text: "Relays, one per line: LISTEN TARGET [LINK-SPEC] [fwd:SPEC] [rev:SPEC]"},
					TextEdit{AssignTo: &u.relaysTE, VScroll: true, MinSize: Size{Height: 120}},
					Label{AssignTo: &u.relaysHelp, Text: "Example: \":9000 remote.example.com:8388\". The optional link spec emulates\r\nimpairments, e.g. \"delay=40ms,loss=1%,rate=20mbit\"."},
					Label{AssignTo: &u.hintLbl},
				}},
				{Title: "Advanced", Layout: VBox{}, Children: []Widget{
					GroupBox{AssignTo: &u.quicGB, Title: "QUIC", Layout: Grid{Columns: 3}, Children: []Widget{
						Label{Text: "Congestion control:", TextAlignment: AlignFar},
						ComboBox{AssignTo: &u.congCB, Model: congestionLabels, ColumnSpan: 2},
						Label{Text: "Max stream window:", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.windowLE, CueBanner: "64MB", ColumnSpan: 2},
						Label{Text: "Chunk size:", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.chunkLE, CueBanner: "16KB", ColumnSpan: 2},
						Label{Text: "Idle timeout (s):", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.timeoutLE, CueBanner: "300", ColumnSpan: 2},
						Label{Text: "Path MTU:", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.mtuLE, CueBanner: "auto (e.g. 1280 for a 1280-byte tunnel)", ColumnSpan: 2},
						Label{},
						CheckBox{AssignTo: &u.noDupCB, Text: "Disable redundant copies on idle paths", ColumnSpan: 2},
						Label{Text: "TLS certificate:", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.certLE, CueBanner: "self-signed (server side)"},
						browse(&u.certLE, "TLS certificate"),
						Label{Text: "TLS key:", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.keyLE},
						browse(&u.keyLE, "TLS private key"),
						Label{Text: "TLS SNI:", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.sniLE, CueBanner: "mpquic", ColumnSpan: 2},
						Label{ColumnSpan: 3, Text: "Certificate files must be readable by LOCAL SERVICE (e.g. keep them in the data folder)."},
					}},
					GroupBox{AssignTo: &u.outboundGB, Title: "Outbound TCP (remote)", Layout: VBox{}, Children: []Widget{
						CheckBox{AssignTo: &u.raceCB, Text: "Race connection attempts (aggressive)"},
						Label{Text: "Opens each connection with up to 5 staggered attempts (at 0, 100, 300, 600, 1000 ms) and keeps\r\n" +
							"the first that connects. For networks or NATs that drop connection setup, where a lost SYN\r\n" +
							"otherwise stalls the connection for 1 s, 3 s or more."},
					}},
					GroupBox{Title: "Logging", Layout: Grid{Columns: 2}, Children: []Widget{
						Label{Text: "Log level:", TextAlignment: AlignFar},
						ComboBox{AssignTo: &u.logCB, Model: logLevels},
						Label{Text: "Statistics interval (s):", TextAlignment: AlignFar},
						LineEdit{AssignTo: &u.statsLE, CueBanner: "off"},
					}},
					VSpacer{},
				}},
				{Title: "Log", Layout: VBox{}, Children: []Widget{
					TextEdit{AssignTo: &u.logTE, ReadOnly: true, VScroll: true, HScroll: true, Font: Font{Family: "Consolas", PointSize: 9}},
					Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
						PushButton{Text: "Refresh", OnClicked: func() { u.lastLog = ""; u.refreshLog() }},
						PushButton{Text: "Open log folder", OnClicked: func() { openFolder(filepath.Dir(winsvc.LogPath(u.currentName()))) }},
						HSpacer{},
					}},
				}},
			}},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				PushButton{AssignTo: &u.applyPB, Text: "Save and apply", OnClicked: u.apply},
				PushButton{AssignTo: &u.startPB, Text: "Start", OnClicked: func() { u.serviceOp("Starting", winsvc.Start) }},
				PushButton{AssignTo: &u.stopPB, Text: "Stop", OnClicked: func() { u.serviceOp("Stopping", winsvc.Stop) }},
				PushButton{AssignTo: &u.restartPB, Text: "Restart", OnClicked: func() { u.serviceOp("Restarting", winsvc.Restart) }},
				HSpacer{},
				PushButton{AssignTo: &u.uninstallPB, Text: "Uninstall...", OnClicked: u.uninstall},
			}},
		},
	}.Create()
}

func (u *ui) currentName() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.name
}

func (u *ui) say(msg string) { u.sbItem.SetText(msg) }

// ---- form <-> widgets ----

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

func congestionIndex(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bbr-aggressive", "aggressive":
		return 1
	case "bbr-conservative", "conservative":
		return 2
	case "cubic", "reno", "newreno":
		return 3
	}
	return 0
}

// crlf converts newlines for the Win32 edit control.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func (u *ui) setForm(f Form) {
	u.roleCB.SetCurrentIndex(max(indexOf([]string{"local", "tunnel", "remote", "relay"}, f.Role), 0))
	u.reverseCB.SetChecked(f.Reverse)
	u.pathsTE.SetText(crlf(f.Paths))
	u.portLE.SetText(f.Port)
	u.pwLE.SetText(f.Password)
	u.udpCB.SetChecked(f.UDP)
	u.laddrLE.SetText(f.LocalAddr)
	u.lportLE.SetText(f.LocalPort)
	u.tunnelLE.SetText(f.Tunnel)
	u.relaysTE.SetText(crlf(f.Relays))
	u.congCB.SetCurrentIndex(congestionIndex(f.Congestion))
	u.windowLE.SetText(f.Window)
	u.chunkLE.SetText(f.Chunk)
	u.timeoutLE.SetText(f.Timeout)
	u.mtuLE.SetText(f.MTU)
	u.statsLE.SetText(f.Stats)
	u.noDupCB.SetChecked(f.NoDup)
	u.raceCB.SetChecked(f.RaceConnect)
	u.certLE.SetText(f.Cert)
	u.keyLE.SetText(f.Key)
	u.sniLE.SetText(f.SNI)
	u.logCB.SetCurrentIndex(max(indexOf(logLevels, f.LogLevel), 0))
	u.updateRole()
}

func (u *ui) readForm() Form {
	role := "local"
	if i := u.roleCB.CurrentIndex(); i >= 0 && i < 4 {
		role = []string{"local", "tunnel", "remote", "relay"}[i]
	}
	cong := ""
	if i := u.congCB.CurrentIndex(); i > 0 && i < len(congestionValues) {
		cong = congestionValues[i]
	}
	level := "info"
	if i := u.logCB.CurrentIndex(); i >= 0 && i < len(logLevels) {
		level = logLevels[i]
	}
	nl := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	return Form{
		Role:        role,
		Reverse:     u.reverseCB.Checked(),
		Paths:       nl(u.pathsTE.Text()),
		Port:        strings.TrimSpace(u.portLE.Text()),
		Password:    u.pwLE.Text(),
		UDP:         u.udpCB.Checked(),
		LocalAddr:   strings.TrimSpace(u.laddrLE.Text()),
		LocalPort:   strings.TrimSpace(u.lportLE.Text()),
		Tunnel:      strings.TrimSpace(u.tunnelLE.Text()),
		Relays:      nl(u.relaysTE.Text()),
		Congestion:  cong,
		Window:      strings.TrimSpace(u.windowLE.Text()),
		Chunk:       strings.TrimSpace(u.chunkLE.Text()),
		Timeout:     strings.TrimSpace(u.timeoutLE.Text()),
		MTU:         strings.TrimSpace(u.mtuLE.Text()),
		Stats:       strings.TrimSpace(u.statsLE.Text()),
		NoDup:       u.noDupCB.Checked(),
		RaceConnect: u.raceCB.Checked(),
		Cert:        strings.TrimSpace(u.certLE.Text()),
		Key:         strings.TrimSpace(u.keyLE.Text()),
		SNI:         strings.TrimSpace(u.sniLE.Text()),
		LogLevel:    level,
	}
}

func (u *ui) dirty() bool { return !reflect.DeepEqual(u.readForm(), u.loaded) }

// updateRole shows the fields that apply to the selected role.
func (u *ui) updateRole() {
	if u.roleCB == nil || u.hintLbl == nil {
		return // still building the window
	}
	f := u.readForm()
	role, rev := f.Role, f.Reverse
	quic := role != "relay"
	local := role == "local" || role == "tunnel"
	show := func(v bool, ws ...walk.Widget) {
		for _, w := range ws {
			w.SetVisible(v)
		}
	}
	show(quic, u.reverseCB, u.pathsLbl, u.pathsTE, u.quicRows)
	show(role == "local" || role == "remote", u.udpCB)
	show(local, u.laddrLbl, u.laddrLE, u.lportLbl, u.lportLE)
	show(role == "tunnel", u.tunnelLbl, u.tunnelLE)
	show(!quic, u.relaysLbl, u.relaysTE, u.relaysHelp)
	show(role == "remote", u.outboundGB)
	u.quicGB.SetEnabled(quic)

	listens := (local && rev) || (role == "remote" && !rev)
	switch {
	case local && !rev:
		u.pathsLbl.SetText("Paths to the remote, one per line: its address and each relay (host[:port]):")
	case local && rev:
		u.pathsLbl.SetText("QUIC listen addresses, one per line (empty: all interfaces):")
	case role == "remote" && !rev:
		u.pathsLbl.SetText("Listen addresses, one per line (empty: all interfaces):")
	case role == "remote" && rev:
		u.pathsLbl.SetText("Paths to the local instance, one per line: its address and each relay (host[:port]):")
	}
	if listens {
		u.portLbl.SetText("Listen port (UDP):")
	} else {
		u.portLbl.SetText("Default port:")
	}

	var hint []string
	cfg, _ := f.Apply(u.baseConfig())
	if local {
		addr := "127.0.0.1:1080"
		if cfg != nil {
			addr = cfgListen(cfg)
		}
		if role == "local" {
			hint = append(hint, "Applications use the SOCKS5 proxy at "+addr+".")
		} else {
			hint = append(hint, "Connections to "+addr+" are forwarded to the tunnel target.")
		}
	}
	if cfg != nil {
		for _, r := range winsvc.FirewallRules(cfg) {
			hint = append(hint, "Windows Firewall: inbound "+r.String()+" will be allowed.")
		}
	}
	u.hintLbl.SetText(strings.Join(hint, "\r\n"))
}

func cfgListen(c *config.Config) string {
	host, port := c.LocalAddress, c.LocalPort
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 1080
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func (u *ui) baseConfig() *config.Config {
	if u.base == nil {
		return &config.Config{}
	}
	return u.base
}

// ---- instances ----

func (u *ui) load(name string) {
	cfg, err := config.Load(winsvc.ConfigPath(name))
	switch {
	case err == nil:
		u.say("Loaded " + winsvc.ConfigPath(name))
	case errors.Is(err, os.ErrNotExist):
		cfg = DefaultConfig()
		u.say("New service " + name + ": fill in the settings, then \"Save and apply\".")
	default:
		walk.MsgBox(u.mw, "MPQUIC", "Cannot load the config: "+err.Error(), walk.MsgBoxIconError)
		cfg = DefaultConfig()
	}
	u.mu.Lock()
	u.name = name
	u.mu.Unlock()
	u.nameCB.SetText(name)
	u.base = cfg
	u.setForm(FormFromConfig(cfg))
	u.loaded = u.readForm()
	u.lastLog = ""
	u.refreshStatus()
	u.refreshLog()
}

func (u *ui) switchTo(name string) {
	if name == "" || strings.EqualFold(name, u.currentName()) {
		return
	}
	if err := winsvc.CheckName(name); err != nil {
		walk.MsgBox(u.mw, "MPQUIC", err.Error(), walk.MsgBoxIconWarning)
		u.nameCB.SetText(u.currentName())
		return
	}
	if u.dirty() && walk.MsgBox(u.mw, "MPQUIC", "Discard the unsaved changes to "+u.currentName()+"?",
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		u.nameCB.SetText(u.currentName())
		return
	}
	u.load(name)
}

// ---- actions ----

func (u *ui) generatePassword() {
	if u.pwLE.Text() != "" && walk.MsgBox(u.mw, "MPQUIC",
		"Replace the password? Both ends must use the same password.", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}
	u.pwLE.SetText(GeneratePassword())
	u.showPwCB.SetChecked(true)
}

func (u *ui) apply() {
	cfg, err := u.readForm().Apply(u.baseConfig())
	if err != nil {
		walk.MsgBox(u.mw, "MPQUIC", "Please check the settings: "+err.Error(), walk.MsgBoxIconWarning)
		return
	}
	name := u.currentName()
	u.run("Applying", func(progress func(string)) error {
		return winsvc.Install(winsvc.InstallOptions{Name: name, Config: cfg, Start: true, Progress: progress})
	}, func() {
		u.base = cfg
		u.loaded = u.readForm()
		if !contains(u.nameCB.Model().([]string), name) {
			u.nameCB.SetModel(append(u.nameCB.Model().([]string), name))
			u.nameCB.SetText(name)
		}
		u.say("Saved and applied: " + name + " is running.")
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func (u *ui) serviceOp(what string, op func(string) error) {
	name := u.currentName()
	u.run(what, func(func(string)) error { return op(name) }, func() { u.say(what + " " + name + ": done.") })
}

func (u *ui) uninstall() {
	name := u.currentName()
	switch walk.MsgBox(u.mw, "Uninstall "+name,
		"Remove the "+name+" service?\r\n\r\nYes: remove it and delete its config and logs.\r\nNo: remove it but keep its config.",
		walk.MsgBoxYesNoCancel|walk.MsgBoxIconQuestion) {
	case walk.DlgCmdYes:
		u.run("Uninstalling", func(p func(string)) error { return winsvc.Uninstall(name, true, p) }, func() { u.load(name) })
	case walk.DlgCmdNo:
		u.run("Uninstalling", func(p func(string)) error { return winsvc.Uninstall(name, false, p) }, func() { u.say("Removed " + name + ".") })
	}
}

// run executes a service operation off the UI thread.
func (u *ui) run(what string, op func(progress func(string)) error, done func()) {
	if u.busy {
		return
	}
	u.busy = true
	u.updateButtons()
	u.say(what + "...")
	progress := func(s string) { u.mw.Synchronize(func() { u.say(s) }) }
	go func() {
		err := op(progress)
		u.mw.Synchronize(func() {
			u.busy = false
			u.refreshStatus()
			u.lastLog = ""
			u.refreshLog()
			if err != nil {
				u.say(what + " failed.")
				walk.MsgBox(u.mw, "MPQUIC", what+" failed: "+err.Error(), walk.MsgBoxIconError)
				return
			}
			done()
		})
	}()
}

func (u *ui) importConfig() {
	dlg := walk.FileDialog{Title: "Import config", Filter: "JSON config (*.json)|*.json|All files (*.*)|*.*"}
	if ok, _ := dlg.ShowOpen(u.mw); !ok {
		return
	}
	cfg, err := config.Load(dlg.FilePath)
	if err != nil {
		walk.MsgBox(u.mw, "MPQUIC", err.Error(), walk.MsgBoxIconError)
		return
	}
	if cfg.Role == "" {
		cfg.Role = u.readForm().Role
	}
	u.base = cfg
	u.setForm(FormFromConfig(cfg))
	u.say("Imported " + dlg.FilePath + " (not applied yet).")
}

func (u *ui) exportConfig() {
	cfg, err := u.readForm().Apply(u.baseConfig())
	if err != nil {
		walk.MsgBox(u.mw, "MPQUIC", "Please check the settings: "+err.Error(), walk.MsgBoxIconWarning)
		return
	}
	dlg := walk.FileDialog{Title: "Export config", Filter: "JSON config (*.json)|*.json", FilePath: u.currentName() + ".json"}
	if ok, _ := dlg.ShowSave(u.mw); !ok {
		return
	}
	path := dlg.FilePath
	if filepath.Ext(path) == "" {
		path += ".json"
	}
	if err := cfg.Save(path); err != nil {
		walk.MsgBox(u.mw, "MPQUIC", err.Error(), walk.MsgBoxIconError)
		return
	}
	u.say("Exported " + path + " (it contains the password).")
}

func (u *ui) about() {
	walk.MsgBox(u.mw, "About MPQUIC",
		"MPQUIC "+u.opt.Version+"\r\nMultipath QUIC proxy (shadowsocks-style).\r\n\r\n"+
			"Binary: "+winsvc.InstalledExe()+"\r\nConfigs and logs: "+winsvc.DataDir()+
			"\r\n\r\nCommand line: mpquic help, mpquic service help", walk.MsgBoxIconInformation)
}

func openFolder(dir string) {
	os.MkdirAll(dir, 0o755)
	exec.Command("explorer.exe", dir).Start()
}

// ---- status ----

func (u *ui) poll(stop <-chan struct{}) {
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		st, err := winsvc.Query(u.currentName())
		u.mw.Synchronize(func() {
			if err == nil {
				u.status = st
				u.showStatus()
			}
			u.refreshLog()
		})
	}
}

func (u *ui) refreshStatus() {
	if st, err := winsvc.Query(u.currentName()); err == nil {
		u.status = st
	}
	u.showStatus()
}

func (u *ui) showStatus() {
	u.statusLbl.SetText(u.status.String())
	u.updateButtons()
}

func (u *ui) updateButtons() {
	st := u.status
	idle := !u.busy
	u.applyPB.SetEnabled(idle)
	u.startPB.SetEnabled(idle && st.Installed && st.String() != "running" && st.String() != "starting")
	u.stopPB.SetEnabled(idle && st.Installed && (st.String() == "running" || st.String() == "starting"))
	u.restartPB.SetEnabled(idle && st.Installed)
	u.uninstallPB.SetEnabled(idle && (st.Installed || fileExists(winsvc.ConfigPath(u.currentName()))))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (u *ui) refreshLog() {
	if u.tabs == nil || u.tabs.CurrentIndex() != logTab {
		return
	}
	text, err := logfile.Tail(winsvc.LogPath(u.currentName()), 256<<10)
	if err != nil {
		text = "No log yet (" + winsvc.LogPath(u.currentName()) + ")."
	}
	if text == u.lastLog {
		return
	}
	u.lastLog = text
	u.logTE.SetText(crlf(text))
	n := u.logTE.TextLength()
	u.logTE.SetTextSelection(n, n)
	u.logTE.ScrollToCaret()
}

// ---- CI ----

// smokeTest round-trips a config through the widgets for every role.
func (u *ui) smokeTest() error {
	for _, js := range []*config.Config{
		{Role: "local", Server: config.StringList{"203.0.113.10", "relay:9000"}, ServerPort: 8443, Password: "pw", Mode: "tcp_and_udp",
			LocalAddress: "0.0.0.0", LocalPort: 1081, Congestion: "bbr-aggressive", Window: 32 << 20, Timeout: 60, LogLevel: "debug", StatsInterval: 30},
		{Role: "tunnel", Server: config.StringList{"a:1"}, Password: "pw", TunnelAddress: "10.0.0.1:22", ChunkSize: 8 << 10, NoDup: true},
		{Role: "remote", Reverse: true, Server: config.StringList{"198.51.100.7:8388"}, Password: "pw", Cert: `C:\c.pem`, Key: `C:\k.pem`, SNI: "x", RaceConnect: true},
		{Role: "relay", Relays: []config.RelayConfig{{Listen: ":9000", Target: "h:8388", Link: "delay=40ms", Rev: "loss=1%"}}, LogLevel: "warn"},
	} {
		want := FormFromConfig(js)
		u.setForm(want)
		got := u.readForm()
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("form round trip for %s:\n got %+v\nwant %+v", js.Role, got, want)
		}
		cfg, err := got.Apply(&config.Config{})
		if err != nil {
			return fmt.Errorf("%s: %w", js.Role, err)
		}
		if !reflect.DeepEqual(cfg, js) {
			return fmt.Errorf("config round trip for %s:\n got %+v\nwant %+v", js.Role, cfg, js)
		}
	}
	u.tabs.SetCurrentIndex(logTab)
	u.refreshLog()
	return nil
}
