package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// --- Config schema ---

type Config struct {
	Project   Project            `toml:"project"`
	Targets   map[string]Target  `toml:"targets"`
	PostBuild []PostBuild        `toml:"post_build"`
	Commands  map[string]Command `toml:"commands"`
}

type Project struct {
	Name       string `toml:"name"`
	BuildCache string `toml:"buildcache"`
}

type Target struct {
	Kind     string              `toml:"kind"`     // "executable" or "object"
	Language string              `toml:"language"` // "c99", "c++20"
	Sources  []string            `toml:"sources"`
	Includes []string            `toml:"includes"`
	Deps     []string            `toml:"deps"`
	Platform map[string]Platform `toml:"platform"`
	Debug    BuildMode           `toml:"debug"`
	Release  BuildMode           `toml:"release"`
}

type Platform struct {
	Includes []string `toml:"includes"`
	LibDirs  []string `toml:"libdirs"`
	Links    []string `toml:"links"`
	Output   string   `toml:"output"`
}

type BuildMode struct {
	Flags []string `toml:"flags"`
}

type PostBuild struct {
	Target     string   `toml:"target"`
	Copy       []string `toml:"copy"`
	RunLinux   string   `toml:"run_linux"`
	RunWindows string   `toml:"run_windows"`
}

type Command struct {
	Description string   `toml:"description"`
	Steps       []string `toml:"steps"`
	Remove      []string `toml:"remove"`
}

// --- Globals ---

const version = "0.1.0"

var (
	cfg      Config
	plat     string
	mode     string // "debug" or "release"
	buildDir string
	cacheDir string
)

func main() {
	// Handle flags that don't need a config file
	cmd := "build"
	mode = "debug"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "--version", "-v":
		fmt.Printf("%s v%s\n", teal("larva"), version)
		return
	case "--help", "-h", "help":
		printHelp()
		return
	}

	// Parse config
	if _, err := toml.DecodeFile("larva.toml", &cfg); err != nil {
		printError("error:", err)
		os.Exit(1)
	}

	// Detect platform
	if runtime.GOOS == "windows" {
		plat = "windows"
	} else {
		plat = "linux"
	}

	if cmd == "release" {
		mode = "release"
		cmd = "build"
	}

	// Resolve build dir from the main executable target
	for _, t := range cfg.Targets {
		if t.Kind == "executable" {
			if p, ok := t.Platform[plat]; ok && p.Output != "" {
				buildDir = p.Output
				break
			}
		}
	}

	// Resolve cache dir for object files (defaults to buildDir if not set)
	cacheDir = cfg.Project.BuildCache
	if cacheDir == "" {
		cacheDir = buildDir
	}

	switch cmd {
	case "build":
		doBuild()
	case "play":
		doBuild()
		doExec()
	case "assets":
		doPostBuild()
	case "clean":
		doClean()
	default:
		// Check custom commands
		if c, ok := cfg.Commands[cmd]; ok {
			doCommand(c)
		} else {
			printUsage()
		}
	}
}

// --- Build logic ---

func doBuild() {
	os.MkdirAll(buildDir, 0o755)
	os.MkdirAll(cacheDir, 0o755)

	// Build all targets in dependency order
	built := map[string][]string{} // target name -> object files
	mainTarget := ""

	// Find the executable target
	for name, t := range cfg.Targets {
		if t.Kind == "executable" {
			mainTarget = name
		}
	}

	// Build dependencies first, then main
	if mainTarget != "" {
		t := cfg.Targets[mainTarget]
		for _, dep := range t.Deps {
			built[dep] = buildTarget(dep, cfg.Targets[dep])
		}
		built[mainTarget] = buildTarget(mainTarget, t)

		// Link
		var allObjects []string
		for _, dep := range t.Deps {
			allObjects = append(allObjects, built[dep]...)
		}
		allObjects = append(allObjects, built[mainTarget]...)
		linkTarget(t, allObjects)
	}

	doPostBuild()
}

func buildTarget(name string, t Target) []string {
	// Resolve sources (expand globs)
	var sources []string
	for _, pat := range t.Sources {
		matches, _ := filepath.Glob(pat)
		sources = append(sources, matches...)
	}
	if len(sources) == 0 {
		printError("warning:", "no sources found for target '"+name+"'")
		return nil
	}

	// Resolve includes
	includes := t.Includes
	if p, ok := t.Platform[plat]; ok {
		includes = append(includes, p.Includes...)
	}

	// Resolve flags
	var flags []string
	if mode == "release" {
		flags = t.Release.Flags
	} else {
		flags = t.Debug.Flags
	}

	// Determine compiler and standard
	compiler, stdFlag := resolveCompiler(t.Language)
	ext := sourceExt(t.Language)

	// Compile each source
	var objects []string
	for _, src := range sources {
		obj := filepath.Join(cacheDir, strings.TrimSuffix(filepath.Base(src), ext)+".o")
		if needsRecompile(src, obj) {
			args := []string{"-c", stdFlag, "-w"}
			args = append(args, flags...)
			for _, inc := range includes {
				args = append(args, "-I", inc)
			}
			args = append(args, src, "-o", obj)
			run(compiler, args...)
		} else {
			printSkip(filepath.Base(src))
		}
		objects = append(objects, obj)
	}
	return objects
}

func linkTarget(t Target, objects []string) {
	output := filepath.Join(buildDir, exeName(cfg.Project.Name))
	args := make([]string, 0, len(objects)+20)
	args = append(args, objects...)
	args = append(args, "-o", output)

	if p, ok := t.Platform[plat]; ok {
		for _, dir := range p.LibDirs {
			args = append(args, "-L", dir)
		}
		for _, link := range p.Links {
			args = append(args, "-l"+link)
		}
	}

	compiler, _ := resolveCompiler(t.Language)
	run(compiler, args...)
}

func doPostBuild() {
	for _, pb := range cfg.PostBuild {
		// Copy files
		for _, pat := range pb.Copy {
			files, _ := filepath.Glob(pat)
			copied := 0
			for _, f := range files {
				dst := filepath.Join(buildDir, filepath.Base(f))
				if needsRecompile(f, dst) {
					data, _ := os.ReadFile(f)
					os.WriteFile(dst, data, 0o644)
					copied++
				}
			}
			if copied > 0 {
				printCopied(copied, pat)
			}
		}

		// Run platform command
		cmdStr := ""
		if plat == "windows" {
			cmdStr = pb.RunWindows
		} else {
			cmdStr = pb.RunLinux
		}
		if cmdStr != "" {
			cmdStr = strings.ReplaceAll(cmdStr, "{output}", buildDir)
			parts := strings.Fields(cmdStr)
			run(parts[0], parts[1:]...)
		}
	}
}

func doExec() {
	exe, _ := filepath.Abs(filepath.Join(buildDir, exeName(cfg.Project.Name)))
	dir, _ := filepath.Abs(buildDir)
	printRunning(exe)
	cmd := exec.Command(exe)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Run()
}

func doClean() {
	if c, ok := cfg.Commands["clean"]; ok {
		for _, dir := range c.Remove {
			os.RemoveAll(dir)
			printRemoved(dir)
		}
	}
	printSuccess("Cleaned.")
}

func doCommand(c Command) {
	for _, step := range c.Steps {
		switch {
		case step == "build":
			doBuild()
		case step == "post_build":
			doPostBuild()
		case strings.HasPrefix(step, "exec:"):
			p := strings.TrimPrefix(step, "exec:")
			p = strings.ReplaceAll(p, "{output}", buildDir)
			p = strings.ReplaceAll(p, "{exe}", exeName(cfg.Project.Name))
			absPath, _ := filepath.Abs(p)
			absDir, _ := filepath.Abs(buildDir)
			cmd := exec.Command(absPath)
			cmd.Dir = absDir
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Stdin = os.Stdin
			cmd.Run()
		}
	}
}

func printHelp() {
	fmt.Printf("%s v%s - a simple C/C++ build system\n\n", teal("larva"), version)
	fmt.Printf("Usage: %s [command]\n\n", teal("larva"))
	fmt.Printf("Commands:\n")
	fmt.Printf("  %s      Debug build (default)\n", teal("build"))
	fmt.Printf("  %s    Optimized release build\n", teal("release"))
	fmt.Printf("  %s      Remove build artifacts\n", teal("clean"))
	fmt.Printf("\n")
	fmt.Printf("Flags:\n")
	fmt.Printf("  %s     Show this help message\n", teal("--help"))
	fmt.Printf("  %s  Show version\n", teal("--version"))
	fmt.Printf("\n")
	fmt.Printf("Additional commands are defined in larva.toml under [commands].\n")
}

func printUsage() {
	fmt.Printf("%s v%s\n\n", teal("larva"), version)
	fmt.Printf("Usage: %s [command]\n\n", teal("larva"))
	fmt.Printf("  %s      Debug build (default)\n", teal("build"))
	fmt.Printf("  %s    Optimized release build\n", teal("release"))
	for name, c := range cfg.Commands {
		fmt.Printf("  %s %s\n", teal(fmt.Sprintf("%-10s", name)), c.Description)
	}
	fmt.Printf("\n")
	fmt.Printf("Run '%s' for more info.\n", teal("larva --help"))
}

// --- Colors (256-color ANSI) ---

const (
	colorReset   = "\033[0m"
	colorTeal    = "\033[38;5;37m"  // main larva color — green/blue teal
	colorDim     = "\033[38;5;245m" // dimmed default prints
	colorBright  = "\033[38;5;48m"  // bright green for success
	colorErr     = "\033[38;5;208m" // orange-red for errors
	colorBold    = "\033[1m"
)

func teal(s string) string   { return colorTeal + s + colorReset }
func dim(s string) string    { return colorDim + s + colorReset }
func bright(s string) string { return colorBold + colorBright + s + colorReset }
func errclr(s string) string { return colorBold + colorErr + s + colorReset }

// --- Print functions ---

func printSkip(file string) {
	fmt.Printf("  %s %s\n", dim("skip"), dim(file))
}

func printCopied(count int, pattern string) {
	fmt.Printf("  %s %d file(s) matching %s\n", teal("copied"), count, pattern)
}

func printRunning(exe string) {
	fmt.Printf("  %s %s\n", teal("running"), exe)
}

func printRemoved(dir string) {
	fmt.Printf("  %s %s\n", teal("removed"), dir)
}

func printSuccess(msg string) {
	fmt.Println(bright(msg))
}

func printError(msg string, detail interface{}) {
	fmt.Fprintf(os.Stderr, "%s %v\n", errclr(msg), detail)
}

func printCmd(name string, args string) {
	fmt.Printf("  %s %s\n", teal(name), dim(args))
}

// --- Helpers ---

func resolveCompiler(lang string) (compiler, stdFlag string) {
	if strings.HasPrefix(lang, "c++") {
		return "g++", "-std=" + lang
	}
	return "gcc", "-std=" + lang
}

func sourceExt(lang string) string {
	if strings.HasPrefix(lang, "c++") {
		return ".cpp"
	}
	return ".c"
}

func needsRecompile(src, obj string) bool {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return true
	}
	objInfo, err := os.Stat(obj)
	if err != nil {
		return true
	}
	return srcInfo.ModTime().After(objInfo.ModTime())
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func run(name string, args ...string) {
	printCmd(name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		printError("FAILED:", err)
		os.Exit(1)
	}
}
