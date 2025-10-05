package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type AppInfo struct {
	Label   string
	Package string
	Main    string
}

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	var errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(out.String() + "\n" + errb.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}

func getPackages(ctx context.Context) ([]string, error) {
	out, err := runCmd(ctx, "pm", "list", "packages", "--user", "0", "-3")
	if err != nil {
		// continue with whatever returned
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	var pkgs []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.TrimPrefix(l, "package:")
		if l != "" {
			pkgs = append(pkgs, l)
		}
	}
	return pkgs, nil
}

func firstLineContaining(s, substr string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		l := sc.Text()
		if strings.Contains(l, substr) {
			return l
		}
	}
	return ""
}

func probePackage(ctx context.Context, pkg string) (*AppInfo, error) {
	// get main activity
	resolveArgs := []string{
		"resolve-activity", "--user", "0",
		"-a", "android.intent.action.MAIN",
		"-c", "android.intent.category.LAUNCHER",
		pkg,
	}
	resOut, _ := runCmd(ctx, "pm", resolveArgs...)
	line := firstLineContaining(resOut, "name=")
	main := ""
	if line != "" {
		idx := strings.Index(line, "name=")
		if idx >= 0 {
			main = strings.TrimSpace(line[idx+len("name="):])
		}
	}

	// get apk path
	pathOut, _ := runCmd(ctx, "pm", "path", pkg, "--user", "0")
	apkPath := ""
	for _, pl := range strings.Split(pathOut, "\n") {
		pl = strings.TrimSpace(pl)
		pl = strings.TrimPrefix(pl, "package:")
		if pl != "" {
			apkPath = pl
			break
		}
	}

	label := ""
	if apkPath != "" {
		aaptOut, err := runCmdBytes(ctx, "aapt", "dump", "badging", apkPath)
		if err == nil && aaptOut != "" {
			sc := bufio.NewScanner(strings.NewReader(aaptOut))
			for sc.Scan() {
				l := sc.Text()
				if strings.Contains(l, "application-label:") {
					start := strings.Index(l, "application-label:")
					if start >= 0 {
						l = l[start+len("application-label:"):]
						l = strings.Trim(l, "'")
						label = l
						break
					}
				}
			}
		}
	}

	// fallback label
	if label == "" {
		label = pkg
	}
	// fallback main
	if main == "" {
		main = "UNKNOWN_MAIN"
	}

	return &AppInfo{
		Label:   label,
		Package: pkg,
		Main:    main,
	}, nil
}

func main() {
	ctx := context.Background()

	pkgs, err := getPackages(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error listing packages:", err)
	}
	if len(pkgs) == 0 {
		fmt.Fprintln(os.Stderr, "no packages found")
		os.Exit(1)
	}

	numWorkers := runtime.NumCPU()
	if numWorkers < 4 {
		numWorkers = 4
	}
	if numWorkers > 16 {
		numWorkers = 16
	}

	in := make(chan string, len(pkgs))
	out := make(chan *AppInfo, len(pkgs))
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pkg := range in {
				pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				info, err := probePackage(pctx, pkg)
				cancel()
				if err == nil && info != nil {
					out <- info
				}
			}
		}()
	}

	for _, p := range pkgs {
		in <- p
	}
	close(in)

	go func() {
		wg.Wait()
		close(out)
	}()

	var apps []*AppInfo
	for a := range out {
		apps = append(apps, a)
	}

	sort.Slice(apps, func(i, j int) bool {
		return strings.ToLower(apps[i].Label) < strings.ToLower(apps[j].Label)
	})

	var fzfInput bytes.Buffer
	for _, a := range apps {
		line := fmt.Sprintf("%s\t%s|%s\n", a.Label, a.Package, a.Main)
		fzfInput.WriteString(line)
	}

	fzfCmd := exec.Command("fzf", "--with-nth=1", "--delimiter=\t", "--layout=reverse")
	fzfCmd.Stdin = &fzfInput

	var chosenBuf bytes.Buffer
	fzfCmd.Stdout = &chosenBuf
	fzfCmd.Stderr = os.Stderr
	if err := fzfCmd.Run(); err != nil {
		os.Exit(1)
	}

	chosen := strings.TrimSpace(chosenBuf.String())
	if chosen == "" {
		os.Exit(1)
	}

	parts := strings.SplitN(chosen, "\t", 2)
	if len(parts) < 2 {
		fmt.Fprintln(os.Stderr, "unexpected selection format")
		os.Exit(1)
	}

	line := parts[1]
	pair := strings.SplitN(line, "|", 2)
	if len(pair) < 2 {
		fmt.Fprintln(os.Stderr, "unexpected package|main format")
		os.Exit(1)
	}

	pkg := pair[0]
	intent := pair[1]

	if intent == "UNKNOWN_MAIN" {
		playstoreURL := "https://play.google.com/store/apps/details?id=" + pkg
		exec.Command("termux-open-url", playstoreURL).Run()
	} else {
		amArgs := []string{"start", "--user", "0", "-n", fmt.Sprintf("%s/%s", pkg, intent)}
		amCmd := exec.Command("am", amArgs...)
		amCmd.Stdout = os.Stdout
		amCmd.Stderr = os.Stderr
		amCmd.Run()
	}
}
