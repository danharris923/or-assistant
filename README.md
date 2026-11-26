# OR Assistant

A small Slax-Assistant-style AI helper that uses **your own OpenRouter key** and runs as an
unprivileged user. One 7.6 MB static Go binary, no dependencies, no third-party backend.

## Install

Download [`or-assistant_0.1.0_amd64.deb`](releases/or-assistant_0.1.0_amd64.deb) (also on the
Releases page), then:

```sh
sudo apt install ./or-assistant_0.1.0_amd64.deb
```

Open **OR Assistant** from the menu, paste your OpenRouter key in Settings, and pick a model.

## How it works

```
Chromium --app window  ──►  or-assistant (guest, 127.0.0.1:random port, per-launch secret)
                               │  runs tool calls as guest: run_command, read_file,
                               │  write_file, edit_file, list_dir
                               ▼
                         https://openrouter.ai/api/v1  ──►  any model you pick on the page
```

- **Model picker** on the page lists every OpenRouter model, with prices and context size, and filters to tool-capable ones by default.
- **Ask first** (on by default): commands and file writes or edits wait for Allow/Deny. Reading files and listing folders don't ask.
- **Refuses to run as root** unless you pass `--allow-root`. The launcher switches from Slax's root desktop to `guest` via `fbliveapp`.
- The local port only answers a browser holding the launch secret (an HttpOnly, SameSite=Strict cookie). Wrong Host headers, cross-site and non-JSON POSTs are refused.
- Key: set on the page (saved in `~/.config/or-assistant/config.json`, mode 0600) or via `OPENROUTER_API_KEY`.
- `OPENROUTER_BASE_URL` points it at any other OpenAI-compatible API, such as a local Ollama at `http://host:11434/v1`.

## Build (on Windows or Linux)

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/or-assistant .
```

## Debian package (Debian, Ubuntu, Mint, Pop!_OS, …)

```sh
python packaging/build_deb.py dist/or-assistant <out-dir> amd64   # builds or-assistant_<version>_amd64.deb
sudo apt install ./or-assistant_0.1.0_amd64.deb                     # installs to /usr/bin, adds menu entry
```

The binary is static, so on non-Debian distros you can just copy `or-assistant` and
`or-assistant-launch` into `~/.local/bin` (or `/usr/local/bin`).

## Install on Slax by hand (as root)

```sh
install -m 0755 dist/or-assistant              /usr/local/bin/or-assistant
install -m 0755 linux/or-assistant-launch      /usr/local/bin/or-assistant-launch
install -D -m 0644 linux/or-assistant.desktop  /usr/local/share/applications/or-assistant.desktop
install -D -m 0644 web/icon.svg                /usr/share/icons/hicolor/scalable/apps/or-assistant.svg
```

Run `or-assistant-launch`, or pick **OR Assistant** from the menu.
`runuser -u guest -- or-assistant stop` stops the background server.
