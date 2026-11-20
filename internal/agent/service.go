package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// 開機（登入）時自動啟動 agent：macOS 是使用者的 LaunchAgent，Linux 是 systemd 的使用者服務。
// 指令：myriagent agent install｜uninstall｜status。PATH 取登入 shell 的（launchd／systemd 給的很短，
// 代理開的指令會找不到 brew、node 等）。

const launchLabel = "dev.clode.myriagent.agent"
const systemdUnit = "myriagent-agent.service"

func serviceMain(cmd string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	switch cmd {
	case "install":
		return installService(exe, args)
	case "uninstall":
		return uninstallService()
	case "status":
		return serviceStatus()
	}
	return fmt.Errorf("不認得的指令：%s", cmd)
}

// loginPath：登入 shell 的 PATH（讀不到就用目前的，再補常見的位置）
func loginPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
		if runtime.GOOS == "linux" {
			shell = "/bin/bash"
		}
	}
	path := os.Getenv("PATH")
	if out, err := exec.Command(shell, "-lc", "printf %s \"$PATH\"").Output(); err == nil && len(out) > 0 {
		path = string(out)
	}
	seen := map[string]bool{}
	var dirs []string
	for _, d := range append(filepath.SplitList(path), searchPaths()...) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, ":")
}

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Exe}}</string>
    <string>agent</string>
{{- range .Args}}
    <string>{{.}}</string>
{{- end}}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>{{.Path}}</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>{{.Log}}</string>
  <key>StandardErrorPath</key><string>{{.Log}}</string>
</dict>
</plist>
`))

var unitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=Clode Myriagent agent（AI 代理 CLI 的本機中樞）
After=network-online.target

[Service]
ExecStart={{.Exe}} agent{{range .Args}} {{.}}{{end}}
Environment=PATH={{.Path}}
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`))

type serviceSpec struct {
	Label, Exe, Path, Log string
	Args                  []string
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xmlEscapeTo(&b, s)
	return b.String()
}

func xmlEscapeTo(b *bytes.Buffer, s string) error {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	_, err := b.WriteString(r.Replace(s))
	return err
}

// renderService 產生服務檔的內容（純函式，測試用）
func renderService(goos string, spec serviceSpec) (string, error) {
	var b bytes.Buffer
	if goos == "darwin" {
		esc := spec
		esc.Exe, esc.Path, esc.Log = xmlEscape(spec.Exe), xmlEscape(spec.Path), xmlEscape(spec.Log)
		esc.Args = nil
		for _, a := range spec.Args {
			esc.Args = append(esc.Args, xmlEscape(a))
		}
		err := plistTemplate.Execute(&b, esc)
		return b.String(), err
	}
	for _, a := range append([]string{spec.Exe, spec.Path}, spec.Args...) {
		if strings.ContainsAny(a, "\n\"") {
			return "", errors.New("路徑裡有換行或引號，systemd 的設定寫不進去")
		}
	}
	err := unitTemplate.Execute(&b, spec)
	return b.String(), err
}

func servicePaths() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist"), nil
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnit), nil
}

func installService(exe string, args []string) error {
	file, err := servicePaths()
	if err != nil {
		return err
	}
	root := dataRootForArgs(args)
	spec := serviceSpec{Label: launchLabel, Exe: exe, Path: loginPath(), Log: filepath.Join(root, "agent.out.log"), Args: args}
	content, err := renderService(runtime.GOOS, spec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	_ = os.MkdirAll(root, 0o700)
	if runtime.GOOS == "darwin" {
		uid := fmt.Sprint(os.Getuid())
		_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+launchLabel).Run()
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			return err
		}
		if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, file).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap：%v %s", err, out)
		}
		fmt.Println("已安裝：登入時自動啟動（" + file + "）")
		return nil
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return err
	}
	for _, a := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", systemdUnit}, {"--user", "restart", systemdUnit}} {
		if out, err := exec.Command("systemctl", a...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s：%v %s", strings.Join(a, " "), err, out)
		}
	}
	fmt.Println("已安裝：登入時自動啟動（" + file + "）")
	fmt.Println("要在沒有人登入時也執行：sudo loginctl enable-linger " + os.Getenv("USER"))
	return nil
}

func dataRootForArgs(args []string) string {
	for i, a := range args {
		if a == "--data" && i+1 < len(args) {
			return pathsDataRoot(args[i+1])
		}
		if strings.HasPrefix(a, "--data=") {
			return pathsDataRoot(strings.TrimPrefix(a, "--data="))
		}
	}
	return pathsDataRoot("")
}

func uninstallService() error {
	file, err := servicePaths()
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).Run()
	} else {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
	}
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	if runtime.GOOS != "darwin" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	fmt.Println("已移除自動啟動")
	return nil
}

func serviceStatus() error {
	file, err := servicePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err != nil {
		fmt.Println("沒有安裝自動啟動")
		return nil
	}
	var out []byte
	if runtime.GOOS == "darwin" {
		out, _ = exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)).CombinedOutput()
	} else {
		out, _ = exec.Command("systemctl", "--user", "status", "--no-pager", systemdUnit).CombinedOutput()
	}
	fmt.Println("服務檔：" + file)
	fmt.Print(string(out))
	return nil
}
