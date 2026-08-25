package main

import (
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

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
	Name       string            `toml:"name"`
	Compiler   string            `toml:"compiler"`
	BuildCache string            `toml:"buildcache"`
	Vars       map[string]string `toml:"vars"`
}

type Target struct {
	Kind           string              `toml:"kind"`     // "executable" or "object"
	Language       string              `toml:"language"` // "c99", "c++20"
	Sources        []string            `toml:"sources"`
	Includes       []string            `toml:"includes"`
	SystemIncludes []string            `toml:"system_includes"`
	Flags          []string            `toml:"flags"`
	Deps           []string            `toml:"deps"`
	Platform       map[string]Platform `toml:"platform"`
	Debug          BuildMode           `toml:"debug"`
	Release        BuildMode           `toml:"release"`
}

type Platform struct {
	Sources          []string `toml:"sources"` // extra source globs, appended to the target's own
	Includes         []string `toml:"includes"`
	SystemIncludes   []string `toml:"system_includes"`
	LibDirs          []string `toml:"libdirs"`
	Links            []string `toml:"links"`
	ReleaseLinkFlags []string `toml:"release_link_flags"` // extra linker flags applied only in release builds
	Output           string   `toml:"output"`
}

type BuildMode struct {
	Flags []string `toml:"flags"`
}

type PostBuild struct {
	Name       string   `toml:"name"` // label in the build summary; defaults to the command name
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
	cfg         Config
	plat        string
	mode        string // "debug" or "release"
	buildDir    string
	cacheDir    string
	projectRoot string // absolute path of the directory containing larva.toml
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

	// Resolve the project root once; every {projectRoot} expansion and
	// executable path is derived from it.
	root, err := os.Getwd()
	if err != nil {
		fatalf("cannot determine working directory: %v", err)
	}
	projectRoot = root

	// Parse config
	if _, err := toml.DecodeFile("larva.toml", &cfg); err != nil {
		fatalf("reading larva.toml: %v", err)
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
	if _, t, ok := findMainExecutable(); ok {
		if p, ok := t.Platform[plat]; ok && p.Output != "" {
			buildDir = p.Output
		}
	}

	// Separate debug and release artifacts into their own subdirectory:
	// build/<os>/<mode>. The <os> component is already baked into the
	// target's output path, so we only append the mode here.
	buildDir = filepath.Join(buildDir, mode)

	// Resolve cache dir for object files. When buildcache is set explicitly,
	// split it by platform and mode (buildCache/<os>/<mode>) so debug and
	// release caches never collide. When unset, it shares the mode-specific
	// build output dir.
	if cfg.Project.BuildCache != "" {
		cacheDir = filepath.Join(cfg.Project.BuildCache, plat, mode)
	} else {
		cacheDir = buildDir
	}

	switch cmd {
	case "build":
		doBuild()
	case "play":
		doBuild()
		doExec()
	case "debug":
		doBuild()
		doDebug()
	case "assets":
		doPostBuild()
	case "clean":
		doClean()
	case "vs":
		doGenerateVS()
	case "lsp":
		doGenerateCompileCommands(true)
	default:
		// Check custom commands
		if c, ok := cfg.Commands[cmd]; ok {
			doCommand(c)
		} else {
			printUsage()
		}
	}
}

// --- Build step timing ---

type buildStep struct {
	name     string
	duration time.Duration
}

var buildSteps []buildStep

func recordStep(name string, d time.Duration) {
	buildSteps = append(buildSteps, buildStep{name, d})
}

func printStepSummary() {
	width := 0
	for _, s := range buildSteps {
		if len(s.name) > width {
			width = len(s.name)
		}
	}
	for _, s := range buildSteps {
		fmt.Printf("  %s %s\n", dim(fmt.Sprintf("%-*s", width, s.name)), teal(formatDuration(s.duration)))
	}
}

// --- Build logic ---

func doBuild() {
	buildStart := time.Now()
	mustMkdir(buildDir)
	mustMkdir(cacheDir)

	mainTarget, t, ok := findMainExecutable()
	if !ok {
		fatalf("no executable target found in larva.toml")
	}

	// Build dependencies first, then the main target.
	built := map[string][]string{} // target name -> object files
	for _, dep := range t.Deps {
		dt, exists := cfg.Targets[dep]
		if !exists {
			fatalf("target %q lists unknown dependency %q", mainTarget, dep)
		}
		stepStart := time.Now()
		built[dep] = buildTarget(dep, dt, false)
		recordStep("compile "+dep, time.Since(stepStart))
	}
	stepStart := time.Now()
	built[mainTarget] = buildTarget(mainTarget, t, true)
	recordStep("compile "+mainTarget, time.Since(stepStart))

	// Link
	var allObjects []string
	for _, dep := range t.Deps {
		allObjects = append(allObjects, built[dep]...)
	}
	allObjects = append(allObjects, built[mainTarget]...)
	stepStart = time.Now()
	linkTarget(t, allObjects)
	recordStep("link", time.Since(stepStart))

	doPostBuild()

	// Keep the compilation database in sync with the sources on every build
	stepStart = time.Now()
	doGenerateCompileCommands(false)
	recordStep("compile_commands", time.Since(stepStart))

	elapsed := time.Since(buildStart)
	printSuccess(fmt.Sprintf("Build succeeded in %s.", formatDuration(elapsed)))
	printStepSummary()
}

func buildTarget(name string, t Target, isMain bool) []string {
	sources := resolveSources(t)
	if len(sources) == 0 {
		// An executable with nothing to compile can't link, so that's fatal;
		// a source-less dependency (e.g. a header-only interface target, or one
		// whose sources are platform-specific) is legitimate — warn and skip it.
		if isMain {
			fatalf("no sources found for executable target %q", name)
		}
		printWarn("no sources found for target %q — skipping", name)
		return nil
	}

	includes, systemIncludes := resolveIncludes(t)
	flags := modeFlags(t)
	baseFlags := expandAll(t.Flags)
	compiler, stdFlag := resolveCompiler(t.Language)

	// Compile each source
	var objects []string
	for _, src := range sources {
		obj := objectPath(src)
		dep := strings.TrimSuffix(obj, ".o") + ".d"
		if needsRecompile(src, obj, dep) {
			mustMkdir(filepath.Dir(obj))
			args := []string{"-c", stdFlag}
			args = append(args, baseFlags...)
			args = append(args, "-MMD", "-MF", dep)
			args = append(args, flags...)
			for _, inc := range includes {
				args = append(args, "-I", inc)
			}
			for _, inc := range systemIncludes {
				args = append(args, "-isystem", inc)
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
		if mode == "release" {
			args = append(args, p.ReleaseLinkFlags...)
		}
	}

	compiler, _ := resolveCompiler(t.Language)
	run(compiler, args...)
}

func doPostBuild() {
	for _, pb := range cfg.PostBuild {
		// Resolve the platform command up front so the summary can name the step.
		cmdStr := ""
		if plat == "windows" {
			cmdStr = pb.RunWindows
		} else {
			cmdStr = pb.RunLinux
		}
		if cmdStr == "" && len(pb.Copy) == 0 {
			continue
		}

		stepStart := time.Now()

		// Copy files
		for _, pat := range pb.Copy {
			files, err := filepath.Glob(pat)
			if err != nil {
				fatalf("invalid copy pattern %q: %v", pat, err)
			}
			copied := 0
			for _, f := range files {
				dst := filepath.Join(buildDir, filepath.Base(f))
				if isNewer(f, dst) {
					data, err := os.ReadFile(f)
					if err != nil {
						fatalf("copying %s: %v", f, err)
					}
					if err := os.WriteFile(dst, data, 0o644); err != nil {
						fatalf("writing %s: %v", dst, err)
					}
					copied++
				}
			}
			if copied > 0 {
				printCopied(copied, pat)
			}
		}

		// Run platform command
		if cmdStr != "" {
			cmdStr = expandVars(cmdStr)
			parts := strings.Fields(cmdStr)
			if len(parts) == 0 {
				// The command expanded to nothing (e.g. an empty variable);
				// there's nothing to run, so skip it rather than crash.
				printWarn("post-build command expanded to empty — skipping")
			} else {
				run(parts[0], parts[1:]...)
			}
		}

		recordStep(postBuildStepName(pb, cmdStr), time.Since(stepStart))
	}
}

// postBuildStepName labels a post-build entry in the build summary: the
// explicit name if given, else the bare command name, else "copy".
func postBuildStepName(pb PostBuild, cmdStr string) string {
	if pb.Name != "" {
		return pb.Name
	}
	if fields := strings.Fields(cmdStr); len(fields) > 0 {
		base := filepath.Base(fields[0])
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	return "copy"
}

func doExec() {
	exe := filepath.Join(absBuildDir(), exeName(cfg.Project.Name))
	printRunning(exe)
	runInteractive(absBuildDir(), exe)
}

func doDebug() {
	exe := filepath.Join(absBuildDir(), exeName(cfg.Project.Name))
	printRunning("gdb " + exe)

	args := []string{"-tui"}
	if plat == "windows" {
		// Give the inferior its own console — avoids STATUS_DLL_INIT_FAILED
		// (0xc0000142) during startup when the program shares gdb's console.
		args = append(args, "-ex", "set new-console on")
	}
	args = append(args, "-ex", "break main", "-ex", "run", exe)

	runInteractive(absBuildDir(), "gdb", args...)
}

func doClean() {
	if c, ok := cfg.Commands["clean"]; ok {
		for _, dir := range c.Remove {
			if err := os.RemoveAll(dir); err != nil {
				fatalf("removing %s: %v", dir, err)
			}
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
			p := expandVars(strings.TrimPrefix(step, "exec:"))
			if !filepath.IsAbs(p) {
				p = filepath.Join(projectRoot, p)
			}
			runInteractive(absBuildDir(), p)
		}
	}
}

func printHelp() {
	fmt.Printf("%s v%s - a simple C/C++ build system\n\n", teal("larva"), version)
	fmt.Printf("Usage: %s [command]\n\n", teal("larva"))
	fmt.Printf("Commands:\n")
	fmt.Printf("  %s      Debug build (default)\n", teal("build"))
	fmt.Printf("  %s    Optimized release build\n", teal("release"))
	fmt.Printf("  %s      Build and launch gdb with a breakpoint at main\n", teal("debug"))
	fmt.Printf("  %s      Remove build artifacts\n", teal("clean"))
	fmt.Printf("  %s         Generate Visual Studio NMake solution\n", teal("vs"))
	fmt.Printf("  %s        Generate compile_commands.json for LSP\n", teal("lsp"))
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
	fmt.Printf("  %s         Generate Visual Studio solution\n", teal("vs"))
	fmt.Printf("  %s        Generate compile_commands.json for LSP\n", teal("lsp"))
	for name, c := range cfg.Commands {
		fmt.Printf("  %s %s\n", teal(fmt.Sprintf("%-10s", name)), c.Description)
	}
	fmt.Printf("\n")
	fmt.Printf("Run '%s' for more info.\n", teal("larva --help"))
}

// --- Colors (256-color ANSI) ---

const (
	colorReset  = "\033[0m"
	colorTeal   = "\033[38;5;37m"  // main larva color — green/blue teal
	colorDim    = "\033[38;5;245m" // dimmed default prints
	colorBright = "\033[38;5;48m"  // bright green for success
	colorErr    = "\033[38;5;208m" // orange-red for errors
	colorBold   = "\033[1m"
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

func printCmd(name string, args string) {
	fmt.Printf("  %s %s\n", teal(name), dim(args))
}

func printWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", colorErr+"warning:"+colorReset, fmt.Sprintf(format, args...))
}

// --- Helpers ---

func resolveCompiler(lang string) (compiler, stdFlag string) {
	isCpp := strings.HasPrefix(lang, "c++")
	switch cfg.Project.Compiler {
	case "clang":
		if isCpp {
			return "clang++", "-std=" + lang
		}
		return "clang", "-std=" + lang
	default:
		if isCpp {
			return "g++", "-std=" + lang
		}
		return "gcc", "-std=" + lang
	}
}

// fatalf prints a clear error message and aborts. All unrecoverable failures
// funnel through here so the user always gets a single, consistent line.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", errclr("error:"), fmt.Sprintf(format, args...))
	os.Exit(1)
}

// mustMkdir creates a directory tree or aborts with a clear error.
func mustMkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("creating %s: %v", dir, err)
	}
}

// runInteractive launches a program with the current stdio attached (used for
// play/debug/exec steps). A failure to start it (e.g. the binary or gdb is
// missing) is reported as a larva error and aborts. If the program starts but
// exits non-zero, that's the program's own status, not a larva error, so we
// don't print "error:" — but we do propagate its exit code so a failing
// exec: step fails the command (and CI sees it) instead of looking like success.
func runInteractive(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fatalf("running %s: %v", name, err)
	}
}

// findMainExecutable returns the project's single executable target. Target
// names are sorted so the choice is deterministic even if a project were to
// declare more than one executable.
func findMainExecutable() (string, Target, bool) {
	names := make([]string, 0, len(cfg.Targets))
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if cfg.Targets[name].Kind == "executable" {
			return name, cfg.Targets[name], true
		}
	}
	return "", Target{}, false
}

// resolveSources expands a target's source globs into concrete file paths,
// including any contributed by the current platform.
func resolveSources(t Target) []string {
	patterns := append([]string{}, t.Sources...)
	if p, ok := t.Platform[plat]; ok {
		patterns = append(patterns, p.Sources...)
	}

	var sources []string
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			fatalf("invalid source pattern %q: %v", pat, err)
		}
		sources = append(sources, matches...)
	}
	return sources
}

// resolveIncludes merges a target's base include paths with its platform-
// specific ones. Fresh slices are returned so the parsed config is never
// mutated.
func resolveIncludes(t Target) (includes, systemIncludes []string) {
	includes = append(includes, t.Includes...)
	systemIncludes = append(systemIncludes, t.SystemIncludes...)
	if p, ok := t.Platform[plat]; ok {
		includes = append(includes, p.Includes...)
		systemIncludes = append(systemIncludes, p.SystemIncludes...)
	}
	return includes, systemIncludes
}

// modeFlags returns the debug or release compile flags for the current build
// mode, variable-expanded. The result is a fresh slice — the parsed config is
// never mutated.
func modeFlags(t Target) []string {
	if mode == "release" {
		return expandAll(t.Release.Flags)
	}
	return expandAll(t.Debug.Flags)
}

// expandAll variable-expands every string in a slice, returning a fresh slice.
func expandAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = expandVars(s)
	}
	return out
}

// objectPath maps a source file to its cached object path. The source's full
// relative path — directory layout and extension — is preserved under cacheDir
// and ".o" is appended, so no two distinct sources ever share an object file:
// foo.c and foo.cpp in one directory become foo.c.o and foo.cpp.o, and files
// with the same name in different directories keep their directories.
func objectPath(src string) string {
	rel := filepath.Clean(src)
	sep := string(filepath.Separator)
	// Turn a "C:" drive prefix into a "C" path component so sources on
	// different volumes don't collide once the leading separators are stripped.
	if vol := filepath.VolumeName(rel); vol != "" {
		rel = strings.TrimRight(vol, `:\/`) + sep + strings.TrimLeft(rel[len(vol):], `/\`)
	}
	rel = strings.TrimLeft(rel, `/\`)
	// Turn every ".." element into "__" so the object always lands under
	// cacheDir even for sources reached via a parent directory.
	rel = strings.ReplaceAll(rel, ".."+sep, "__"+sep)
	return filepath.Join(cacheDir, rel+".o")
}

// isDriveLetter reports whether b is an ASCII letter, i.e. a Windows drive
// letter as in "C:".
func isDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// absBuildDir returns the build output directory as an absolute path.
func absBuildDir() string {
	if filepath.IsAbs(buildDir) {
		return buildDir
	}
	return filepath.Join(projectRoot, buildDir)
}

func isNewer(src, dst string) bool {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return true
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		return true
	}
	return srcInfo.ModTime().After(dstInfo.ModTime())
}

func needsRecompile(src, obj, dep string) bool {
	objInfo, err := os.Stat(obj)
	if err != nil {
		return true
	}
	objTime := objInfo.ModTime()

	// Check source file
	srcInfo, err := os.Stat(src)
	if err != nil {
		return true
	}
	if srcInfo.ModTime().After(objTime) {
		return true
	}

	// Check header dependencies from .d file
	for _, h := range parseDeps(dep) {
		if hInfo, err := os.Stat(h); err == nil && hInfo.ModTime().After(objTime) {
			return true
		}
	}

	return false
}

func parseDeps(depFile string) []string {
	data, err := os.ReadFile(depFile)
	if err != nil {
		return nil
	}

	// .d format: "target: dep1 dep2 dep3 ..."
	// Continuations use backslash-newline
	content := strings.ReplaceAll(string(data), "\\\n", " ")
	content = strings.ReplaceAll(content, "\\\r\n", " ")

	// Strip the "target:" prefix. On Windows the target is an object path that
	// may itself begin with a drive letter ("C:\cache\x.o: dep.h ..."), so skip
	// a leading "X:" before searching for the colon that separates target from
	// deps — otherwise we'd split on the drive colon and drop every dependency.
	content = strings.TrimLeft(content, " \t\r\n")
	start := 0
	if len(content) >= 2 && content[1] == ':' && isDriveLetter(content[0]) {
		start = 2
	}
	if idx := strings.IndexByte(content[start:], ':'); idx >= 0 {
		content = content[start+idx+1:]
	}

	var deps []string
	for _, d := range strings.Fields(content) {
		deps = append(deps, d)
	}
	return deps
}

func expandVars(s string) string {
	s = strings.ReplaceAll(s, "{projectRoot}", filepath.ToSlash(projectRoot))
	s = strings.ReplaceAll(s, "{output}", buildDir)
	s = strings.ReplaceAll(s, "{exe}", exeName(cfg.Project.Name))
	for k, v := range cfg.Project.Vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	totalSeconds := int(d.Seconds())
	if totalSeconds < 60 {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%dm %ds", minutes, seconds)
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
		fatalf("%s failed: %v", name, err)
	}
}

// --- compile_commands.json Generation ---

type CompileCommand struct {
	Directory string   `json:"directory"`
	Arguments []string `json:"arguments"`
	File      string   `json:"file"`
}

func doGenerateCompileCommands(verbose bool) {
	var commands []CompileCommand

	for _, name := range targetBuildOrder() {
		t := cfg.Targets[name]
		compiler, stdFlag := resolveCompiler(t.Language)
		sources := resolveSources(t)
		includes, systemIncludes := resolveIncludes(t)
		flags := modeFlags(t)
		baseFlags := expandAll(t.Flags)

		for _, src := range sources {
			src = filepath.ToSlash(src)
			var args []string
			args = append(args, compiler, "-c", stdFlag)
			args = append(args, baseFlags...)
			args = append(args, flags...)
			for _, inc := range includes {
				args = append(args, "-I", filepath.ToSlash(inc))
			}
			for _, inc := range systemIncludes {
				args = append(args, "-isystem", filepath.ToSlash(inc))
			}
			args = append(args, src)

			commands = append(commands, CompileCommand{
				Directory: filepath.ToSlash(projectRoot),
				Arguments: args,
				File:      src,
			})
		}
	}

	data, err := json.MarshalIndent(commands, "", "  ")
	if err != nil {
		fatalf("generating compile_commands.json: %v", err)
	}

	if err := os.WriteFile("compile_commands.json", data, 0o644); err != nil {
		fatalf("writing compile_commands.json: %v", err)
	}
	if verbose {
		printSuccess("Generated compile_commands.json")
	}
}

func targetBuildOrder() []string {
	name, t, ok := findMainExecutable()
	if !ok {
		// Fallback: all targets, sorted for a deterministic order.
		order := make([]string, 0, len(cfg.Targets))
		for n := range cfg.Targets {
			order = append(order, n)
		}
		sort.Strings(order)
		return order
	}
	order := make([]string, 0, len(t.Deps)+1)
	order = append(order, t.Deps...)
	order = append(order, name)
	return order
}

// --- VS Solution Generation ---

func doGenerateVS() {
	// Find the executable target
	_, mainTarget, ok := findMainExecutable()
	if !ok {
		fatalf("no executable target found in larva.toml")
	}

	projectName := cfg.Project.Name
	guid := projectGUID(projectName)

	// Collect include paths from main target + deps (windows platform), deduplicated
	var includes []string
	seenInc := map[string]bool{}
	addInc := func(path string) {
		if !seenInc[path] {
			seenInc[path] = true
			includes = append(includes, path)
		}
	}
	for _, inc := range mainTarget.Includes {
		addInc(inc)
	}
	for _, inc := range mainTarget.SystemIncludes {
		addInc(inc)
	}
	if p, ok := mainTarget.Platform["windows"]; ok {
		for _, inc := range p.Includes {
			addInc(inc)
		}
		for _, inc := range p.SystemIncludes {
			addInc(inc)
		}
	}
	for _, dep := range mainTarget.Deps {
		if dt, ok := cfg.Targets[dep]; ok {
			for _, inc := range dt.Includes {
				addInc(inc)
			}
			for _, inc := range dt.SystemIncludes {
				addInc(inc)
			}
			if p, ok := dt.Platform["windows"]; ok {
				for _, inc := range p.Includes {
					addInc(inc)
				}
				for _, inc := range p.SystemIncludes {
					addInc(inc)
				}
			}
		}
	}

	// Convert to backslash paths and join with semicolons for VS
	var vsIncludes []string
	for _, inc := range includes {
		vsIncludes = append(vsIncludes, filepath.FromSlash(inc))
	}
	includeStr := strings.Join(vsIncludes, ";")

	// Collect preprocessor definitions from -D flags
	collectDefines := func(flags []string) string {
		var defs []string
		for _, f := range flags {
			if strings.HasPrefix(f, "-D") {
				def := f[2:]
				if strings.Contains(def, "{projectRoot}") {
					continue
				}
				defs = append(defs, def)
			}
		}
		return strings.Join(defs, ";")
	}
	debugDefs := collectDefines(mainTarget.Debug.Flags)
	releaseDefs := collectDefines(mainTarget.Release.Flags)

	// Collect source files from main target and all deps
	var compileFiles, headerFiles []string
	seen := map[string]bool{}

	addSources := func(t Target) {
		for _, m := range resolveSources(t) {
			m = filepath.Clean(m)
			if seen[m] {
				continue
			}
			seen[m] = true
			ext := strings.ToLower(filepath.Ext(m))
			switch ext {
			case ".cpp", ".cc", ".cxx", ".c":
				compileFiles = append(compileFiles, m)
			case ".h", ".hpp":
				headerFiles = append(headerFiles, m)
			}
		}
	}

	for _, dep := range mainTarget.Deps {
		if dt, ok := cfg.Targets[dep]; ok {
			addSources(dt)
		}
	}
	addSources(mainTarget)

	// Scan include directories for header files
	for _, inc := range includes {
		for _, pattern := range []string{"*.h", "*.hpp"} {
			matches, _ := filepath.Glob(filepath.Join(inc, pattern))
			for _, m := range matches {
				m = filepath.Clean(m)
				if !seen[m] {
					seen[m] = true
					headerFiles = append(headerFiles, m)
				}
			}
		}
	}

	// Resolve output exe paths. Debug and release land in their own
	// subdirectory (build/<os>/<mode>), matching the layout larva builds into.
	debugOutputExe := filepath.FromSlash(filepath.Join("debug", projectName+".exe"))
	releaseOutputExe := filepath.FromSlash(filepath.Join("release", projectName+".exe"))
	if p, ok := mainTarget.Platform["windows"]; ok && p.Output != "" {
		debugOutputExe = filepath.FromSlash(filepath.Join(p.Output, "debug", projectName+".exe"))
		releaseOutputExe = filepath.FromSlash(filepath.Join(p.Output, "release", projectName+".exe"))
	}

	// Write .vcxproj
	vcxprojPath := projectName + ".vcxproj"
	vcxproj := generateVcxproj(projectName, guid, includeStr, debugDefs, releaseDefs, debugOutputExe, releaseOutputExe, compileFiles, headerFiles)
	if err := os.WriteFile(vcxprojPath, []byte(vcxproj), 0o644); err != nil {
		fatalf("writing %s: %v", vcxprojPath, err)
	}

	// Write .sln
	slnPath := projectName + ".sln"
	sln := generateSln(projectName, guid, vcxprojPath)
	if err := os.WriteFile(slnPath, []byte(sln), 0o644); err != nil {
		fatalf("writing %s: %v", slnPath, err)
	}

	printSuccess("Generated Visual Studio solution:")
	fmt.Printf("  %s\n", teal(slnPath))
	fmt.Printf("  %s\n", teal(vcxprojPath))
}

func projectGUID(name string) string {
	h := md5.Sum([]byte(name))
	return fmt.Sprintf("{%02X%02X%02X%02X-%02X%02X-%02X%02X-%02X%02X-%02X%02X%02X%02X%02X%02X}",
		h[0], h[1], h[2], h[3], h[4], h[5], h[6], h[7],
		h[8], h[9], h[10], h[11], h[12], h[13], h[14], h[15])
}

func generateVcxproj(name, guid, includes, debugDefs, releaseDefs, debugOutput, releaseOutput string, compileFiles, headerFiles []string) string {
	var b strings.Builder

	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	b.WriteString("<Project DefaultTargets=\"Build\" xmlns=\"http://schemas.microsoft.com/developer/msbuild/2003\">\n")

	// Project configurations
	b.WriteString("  <ItemGroup Label=\"ProjectConfigurations\">\n")
	for _, conf := range []string{"Debug", "Release"} {
		b.WriteString(fmt.Sprintf("    <ProjectConfiguration Include=\"%s|x64\">\n", conf))
		b.WriteString(fmt.Sprintf("      <Configuration>%s</Configuration>\n", conf))
		b.WriteString("      <Platform>x64</Platform>\n")
		b.WriteString("    </ProjectConfiguration>\n")
	}
	b.WriteString("  </ItemGroup>\n")

	// Globals
	b.WriteString("  <PropertyGroup Label=\"Globals\">\n")
	b.WriteString("    <VCProjectVersion>17.0</VCProjectVersion>\n")
	b.WriteString(fmt.Sprintf("    <ProjectGuid>%s</ProjectGuid>\n", guid))
	b.WriteString("    <Keyword>MakeFileProj</Keyword>\n")
	b.WriteString(fmt.Sprintf("    <ProjectName>%s</ProjectName>\n", name))
	b.WriteString("  </PropertyGroup>\n")

	b.WriteString("  <Import Project=\"$(VCTargetsPath)\\Microsoft.Cpp.Default.props\" />\n")

	// Configuration property groups
	for _, conf := range []struct {
		name  string
		debug bool
	}{{"Debug", true}, {"Release", false}} {
		b.WriteString(fmt.Sprintf("  <PropertyGroup Condition=\"'$(Configuration)|$(Platform)'=='%s|x64'\" Label=\"Configuration\">\n", conf.name))
		b.WriteString("    <ConfigurationType>Makefile</ConfigurationType>\n")
		if conf.debug {
			b.WriteString("    <UseDebugLibraries>true</UseDebugLibraries>\n")
		} else {
			b.WriteString("    <UseDebugLibraries>false</UseDebugLibraries>\n")
		}
		b.WriteString("    <PlatformToolset>v143</PlatformToolset>\n")
		b.WriteString("  </PropertyGroup>\n")
	}

	b.WriteString("  <Import Project=\"$(VCTargetsPath)\\Microsoft.Cpp.props\" />\n")

	// NMake settings — Debug
	b.WriteString("  <PropertyGroup Condition=\"'$(Configuration)|$(Platform)'=='Debug|x64'\">\n")
	b.WriteString("    <NMakeBuildCommandLine>larva build</NMakeBuildCommandLine>\n")
	b.WriteString(fmt.Sprintf("    <NMakeOutput>%s</NMakeOutput>\n", debugOutput))
	b.WriteString("    <NMakeCleanCommandLine>larva clean</NMakeCleanCommandLine>\n")
	b.WriteString("    <NMakeReBuildCommandLine>larva clean &amp;&amp; larva build</NMakeReBuildCommandLine>\n")
	b.WriteString(fmt.Sprintf("    <NMakeIncludeSearchPath>%s</NMakeIncludeSearchPath>\n", includes))
	b.WriteString(fmt.Sprintf("    <NMakePreprocessorDefinitions>%s</NMakePreprocessorDefinitions>\n", debugDefs))
	b.WriteString("  </PropertyGroup>\n")

	// NMake settings — Release
	b.WriteString("  <PropertyGroup Condition=\"'$(Configuration)|$(Platform)'=='Release|x64'\">\n")
	b.WriteString("    <NMakeBuildCommandLine>larva release</NMakeBuildCommandLine>\n")
	b.WriteString(fmt.Sprintf("    <NMakeOutput>%s</NMakeOutput>\n", releaseOutput))
	b.WriteString("    <NMakeCleanCommandLine>larva clean</NMakeCleanCommandLine>\n")
	b.WriteString("    <NMakeReBuildCommandLine>larva clean &amp;&amp; larva release</NMakeReBuildCommandLine>\n")
	b.WriteString(fmt.Sprintf("    <NMakeIncludeSearchPath>%s</NMakeIncludeSearchPath>\n", includes))
	b.WriteString(fmt.Sprintf("    <NMakePreprocessorDefinitions>%s</NMakePreprocessorDefinitions>\n", releaseDefs))
	b.WriteString("  </PropertyGroup>\n")

	// Source files (ClCompile)
	if len(compileFiles) > 0 {
		b.WriteString("  <ItemGroup>\n")
		for _, f := range compileFiles {
			b.WriteString(fmt.Sprintf("    <ClCompile Include=\"%s\" />\n", filepath.FromSlash(f)))
		}
		b.WriteString("  </ItemGroup>\n")
	}

	// Header files (ClInclude)
	if len(headerFiles) > 0 {
		b.WriteString("  <ItemGroup>\n")
		for _, f := range headerFiles {
			b.WriteString(fmt.Sprintf("    <ClInclude Include=\"%s\" />\n", filepath.FromSlash(f)))
		}
		b.WriteString("  </ItemGroup>\n")
	}

	b.WriteString("  <Import Project=\"$(VCTargetsPath)\\Microsoft.Cpp.targets\" />\n")
	b.WriteString("</Project>\n")

	return b.String()
}

func generateSln(name, projectGuid, vcxprojPath string) string {
	typeGUID := "{8BC9CEB8-8B4A-11D0-8D11-00A0C91BC942}"

	var b strings.Builder
	b.WriteString("\xEF\xBB\xBF\r\n") // UTF-8 BOM
	b.WriteString("Microsoft Visual Studio Solution File, Format Version 12.00\r\n")
	b.WriteString("# Visual Studio Version 17\r\n")
	b.WriteString("VisualStudioVersion = 17.0.31903.59\r\n")
	b.WriteString("MinimumVisualStudioVersion = 10.0.40219.1\r\n")
	b.WriteString(fmt.Sprintf("Project(\"%s\") = \"%s\", \"%s\", \"%s\"\r\n", typeGUID, name, vcxprojPath, projectGuid))
	b.WriteString("EndProject\r\n")
	b.WriteString("Global\r\n")
	b.WriteString("\tGlobalSection(SolutionConfigurationPlatforms) = preSolution\r\n")
	b.WriteString("\t\tDebug|x64 = Debug|x64\r\n")
	b.WriteString("\t\tRelease|x64 = Release|x64\r\n")
	b.WriteString("\tEndGlobalSection\r\n")
	b.WriteString("\tGlobalSection(ProjectConfigurationPlatforms) = postSolution\r\n")
	b.WriteString(fmt.Sprintf("\t\t%s.Debug|x64.ActiveCfg = Debug|x64\r\n", projectGuid))
	b.WriteString(fmt.Sprintf("\t\t%s.Debug|x64.Build.0 = Debug|x64\r\n", projectGuid))
	b.WriteString(fmt.Sprintf("\t\t%s.Release|x64.ActiveCfg = Release|x64\r\n", projectGuid))
	b.WriteString(fmt.Sprintf("\t\t%s.Release|x64.Build.0 = Release|x64\r\n", projectGuid))
	b.WriteString("\tEndGlobalSection\r\n")
	b.WriteString("\tGlobalSection(SolutionProperties) = preSolution\r\n")
	b.WriteString("\t\tHideSolutionNode = FALSE\r\n")
	b.WriteString("\tEndGlobalSection\r\n")
	b.WriteString("EndGlobal\r\n")

	return b.String()
}
