package main

import (
	"crypto/md5"
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
	Name       string            `toml:"name"`
	Compiler   string            `toml:"compiler"`
	BuildCache string            `toml:"buildcache"`
	Vars       map[string]string `toml:"vars"`
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
	case "vs":
		doGenerateVS()
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
	printSuccess("Build succeeded.")
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

	for i, f := range flags {
		flags[i] = expandVars(f)
	}

	// Determine compiler and standard
	compiler, stdFlag := resolveCompiler(t.Language)
	ext := sourceExt(t.Language)

	// Compile each source
	var objects []string
	for _, src := range sources {
		obj := filepath.Join(cacheDir, strings.TrimSuffix(filepath.Base(src), ext)+".o")
		dep := strings.TrimSuffix(obj, ".o") + ".d"
		if needsRecompile(src, obj, dep) {
			args := []string{"-c", stdFlag, "-w", "-MMD", "-MF", dep}
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
				if isNewer(f, dst) {
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
			cmdStr = expandVars(cmdStr)
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
			p = expandVars(p)
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
	fmt.Printf("  %s         Generate Visual Studio NMake solution\n", teal("vs"))
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

func sourceExt(lang string) string {
	if strings.HasPrefix(lang, "c++") {
		return ".cpp"
	}
	return ".c"
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

	// Strip the "target:" prefix
	if idx := strings.Index(content, ":"); idx >= 0 {
		content = content[idx+1:]
	}

	var deps []string
	for _, d := range strings.Fields(content) {
		deps = append(deps, d)
	}
	return deps
}

func expandVars(s string) string {
	cwd, _ := os.Getwd()
	s = strings.ReplaceAll(s, "{projectRoot}", filepath.ToSlash(cwd))
	s = strings.ReplaceAll(s, "{output}", buildDir)
	s = strings.ReplaceAll(s, "{exe}", exeName(cfg.Project.Name))
	for k, v := range cfg.Project.Vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
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

// --- VS Solution Generation ---

func doGenerateVS() {
	// Find the executable target
	var mainName string
	var mainTarget Target
	for name, t := range cfg.Targets {
		if t.Kind == "executable" {
			mainName = name
			mainTarget = t
			break
		}
	}
	if mainName == "" {
		printError("error:", "no executable target found")
		os.Exit(1)
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
	if p, ok := mainTarget.Platform["windows"]; ok {
		for _, inc := range p.Includes {
			addInc(inc)
		}
	}
	for _, dep := range mainTarget.Deps {
		if dt, ok := cfg.Targets[dep]; ok {
			for _, inc := range dt.Includes {
				addInc(inc)
			}
			if p, ok := dt.Platform["windows"]; ok {
				for _, inc := range p.Includes {
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
		for _, pat := range t.Sources {
			matches, _ := filepath.Glob(pat)
			for _, m := range matches {
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

	// Resolve output exe path
	outputExe := projectName + ".exe"
	if p, ok := mainTarget.Platform["windows"]; ok && p.Output != "" {
		outputExe = filepath.FromSlash(filepath.Join(p.Output, projectName+".exe"))
	}

	// Write .vcxproj
	vcxprojPath := projectName + ".vcxproj"
	vcxproj := generateVcxproj(projectName, guid, includeStr, debugDefs, releaseDefs, outputExe, compileFiles, headerFiles)
	os.WriteFile(vcxprojPath, []byte(vcxproj), 0o644)

	// Write .sln
	slnPath := projectName + ".sln"
	sln := generateSln(projectName, guid, vcxprojPath)
	os.WriteFile(slnPath, []byte(sln), 0o644)

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

func generateVcxproj(name, guid, includes, debugDefs, releaseDefs, output string, compileFiles, headerFiles []string) string {
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
	b.WriteString(fmt.Sprintf("    <NMakeOutput>%s</NMakeOutput>\n", output))
	b.WriteString("    <NMakeCleanCommandLine>larva clean</NMakeCleanCommandLine>\n")
	b.WriteString("    <NMakeReBuildCommandLine>larva clean &amp;&amp; larva build</NMakeReBuildCommandLine>\n")
	b.WriteString(fmt.Sprintf("    <NMakeIncludeSearchPath>%s</NMakeIncludeSearchPath>\n", includes))
	b.WriteString(fmt.Sprintf("    <NMakePreprocessorDefinitions>%s</NMakePreprocessorDefinitions>\n", debugDefs))
	b.WriteString("  </PropertyGroup>\n")

	// NMake settings — Release
	b.WriteString("  <PropertyGroup Condition=\"'$(Configuration)|$(Platform)'=='Release|x64'\">\n")
	b.WriteString("    <NMakeBuildCommandLine>larva release</NMakeBuildCommandLine>\n")
	b.WriteString(fmt.Sprintf("    <NMakeOutput>%s</NMakeOutput>\n", output))
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
