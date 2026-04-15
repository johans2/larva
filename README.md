# larva

A small C/C++ build system driven by a single `larva.toml` file. Written in Go,
distributed as a single binary, intended for projects where CMake is overkill
but a shell script is underkill.

## Requirements

**To install larva:**
- [Go](https://go.dev/dl/) (any recent version)

**To use larva on a C/C++ project:**
- `gcc` / `g++` (default), or `clang` / `clang++` (set `compiler = "clang"`)
- `gdb` — only required for `larva debug`
- `devenv` / `msbuild` — only if you use the generated solution from `larva vs`

**Platform notes:**
- **Windows:** tested with MSYS2 / MinGW toolchains. `gdb -tui` needs a real
  terminal (Windows Terminal, cmd, an MSYS2 shell); some embedded terminals
  render the TUI poorly.
- **Linux:** any distro-provided `gcc` + `gdb` works.

## Installation

Clone the repo, then from the project directory:

```sh
# Linux / macOS
./install.sh

# Windows
install.bat
```

Both scripts build `larva` with Go and drop it into `~/.local/bin`. Add that
directory to your `PATH` if it isn't already.

## Commands

Run `larva` from any directory containing a `larva.toml`.

| Command         | What it does                                                   |
|-----------------|----------------------------------------------------------------|
| `larva build`   | Debug build (default when no command is given).                |
| `larva release` | Optimized release build.                                       |
| `larva debug`   | Debug build, then launch `gdb -tui` with a breakpoint at `main` and auto-run. |
| `larva play`    | Debug build, then run the produced executable.                 |
| `larva assets`  | Run the `[[post_build]]` steps without recompiling.            |
| `larva clean`   | Remove build artifacts (driven by the `clean` entry in `[commands]`). |
| `larva vs`      | Generate a Visual Studio NMake-based `.sln` + `.vcxproj`.      |
| `larva lsp`     | Generate `compile_commands.json` for clangd and other LSPs.    |
| `larva <name>`  | Run a custom command defined under `[commands.<name>]`.        |

Flags: `--help` / `-h`, `--version` / `-v`.

## Example: `larva.toml`

```toml
[project]
name     = "myapp"
compiler = "gcc"           # or "clang"
buildcache = ".cache"      # where .o and .d files live (defaults to output dir)

[project.vars]
# Available as {assets} inside flags and post-build commands
assets = "assets"

# --- The main executable target ---

[targets.myapp]
kind     = "executable"
language = "c++20"         # also: "c99", "c11", "c++17", etc.
sources  = ["src/*.cpp"]
includes = ["src", "include"]
system_includes = ["third_party/glm"]   # -isystem (suppresses warnings)
flags    = ["-Wall", "-Wextra"]
deps     = ["util"]                     # other targets to link in

[targets.myapp.debug]
flags = ["-g", "-O0", "-DDEBUG"]

[targets.myapp.release]
flags = ["-O2", "-DNDEBUG"]

[targets.myapp.platform.linux]
libdirs = ["/usr/local/lib"]
links   = ["m", "pthread"]
output  = "build/linux"

[targets.myapp.platform.windows]
libdirs = ["C:/libs"]
links   = ["user32", "gdi32"]
output  = "build/win"

# --- A static dependency target ---

[targets.util]
kind     = "object"
language = "c++20"
sources  = ["util/*.cpp"]
includes = ["util"]

[targets.util.debug]
flags = ["-g", "-O0"]

[targets.util.release]
flags = ["-O2"]

# --- Post-build steps ---

[[post_build]]
target        = "myapp"
copy          = ["{assets}/*.png", "{assets}/shaders/*.glsl"]
run_linux     = "strip {output}/{exe}"
run_windows   = ""

# --- Custom commands ---

[commands.clean]
description = "Remove build + cache directories"
remove      = ["build", ".cache"]

[commands.bench]
description = "Build and run the benchmark binary"
steps = [
  "build",
  "post_build",
  "exec:build/linux/myapp",
]
```

## Schema reference

**`[project]`**
- `name` — executable name (`.exe` suffix added automatically on Windows).
- `compiler` — `gcc` (default) or `clang`.
- `buildcache` — where `.o` / `.d` files are cached. Defaults to the target's `output` dir.
- `vars` — user-defined substitutions. Referenced as `{name}` in flags and commands.

**`[targets.<name>]`**
- `kind` — `executable` (one per project) or `object` (dependency).
- `language` — passed to `-std=...`. E.g. `c99`, `c11`, `c++17`, `c++20`.
- `sources` — glob patterns (e.g. `src/*.cpp`).
- `includes` — `-I` paths.
- `system_includes` — `-isystem` paths. Warnings from these headers are suppressed.
- `flags` — extra compile flags always applied.
- `deps` — names of other targets to link in.
- `debug.flags` / `release.flags` — mode-specific flags.
- `platform.<linux|windows>.{includes, system_includes, libdirs, links, output}` —
  platform-specific extras. `links` are plain library names (`-l` is added).

**`[[post_build]]`**
- `target` — which target this runs after.
- `copy` — glob patterns, copied into the output dir (skipped if dest is up to date).
- `run_linux` / `run_windows` — shell command run after the copy step.

**`[commands.<name>]`**
- `description` — shown in `larva` usage output.
- `steps` — run in order. Each step is one of:
  - `build` — same as `larva build`.
  - `post_build` — run post-build steps only.
  - `exec:<path>` — run an executable (cwd is the build output dir).
- `remove` — directories to delete. Used by `larva clean`.

## Variable expansion

Available anywhere a flag or command string is used:
- `{projectRoot}` — absolute path to the directory containing `larva.toml`.
- `{output}` — the resolved output directory for the current platform.
- `{exe}` — the final executable filename (includes `.exe` on Windows).
- `{name}` — any key from `[project.vars]`.

## How builds work

- Object files land in `buildcache` (or `output` if unset).
- Incremental: each source has a `.d` file generated with `-MMD`, so header
  edits trigger re-compilation of just the affected translation units.
- Dependencies (`deps`) are built first, then the main target, then linked.
- `post_build` runs after link.
- A non-zero exit from any compiler / linker / command aborts the build.
