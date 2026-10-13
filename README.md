# voice-io

Private-team, source-installed English voice input for Linux GNOME and macOS. Three recording modes share offline speech recognition; downloading models, enabling desktop integrations, and configuring a meeting-summary API are explicit choices.

## Install and start

Use Python **3.12+** with `venv` and `pip`, a working microphone, and enough disk space for downloaded model weights. From this checkout:

```sh
./scripts/install                 # package and ~/.local/bin/voice-io.sh
# Linux GNOME only, instead: ./scripts/install --desktop
~/.local/bin/voice-io.sh models install
~/.local/bin/voice-io.sh models verify
~/.local/bin/voice-io.sh start
~/.local/bin/voice-io.sh settings
~/.local/bin/voice-io.sh stop
```

For Meeting or Collaborative, additionally run `~/.local/bin/voice-io.sh models install --meeting` and `~/.local/bin/voice-io.sh models verify --meeting` before recording. These commands explicitly fetch pinned offline weights; starting the daemon does not download models. Exact upstream sources, revisions, required files, and hashes are in [`voice_io/install_models.py`](voice_io/install_models.py). Review [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), including separate model terms, before distributing software or weights.

**Linux GNOME:** Install GTK 3 Python bindings (`python3-gi`, `python3-cairo`, `gir1.2-gtk-3.0`), PortAudio, `wl-clipboard`, and an input sender (`ydotool` or `wtype`); `aplay` is optional for sounds. `--desktop` installs an XDG launcher, SVG icon, optional systemd user-unit template, and GNOME Wayland focus bridge, but enables neither the service nor the extension. To use the bridge, run `gnome-extensions enable gnome_focus@voice-io` (log out/in first if GNOME has not discovered the extension), then restart the daemon. Wayland input injection may require `/dev/uinput` permissions; [`conf/99-voiceio-uinput.rules`](conf/99-voiceio-uinput.rules) grants broad input-device access to the `input` group, so review that privilege before installing it.

**macOS:** Install PortAudio (for example with Homebrew). The installer adds Cocoa/Quartz Python dependencies; grant the installed Python executable Microphone, Accessibility, and Input Monitoring access in System Settings → Privacy & Security, then restart. Launch `~/.local/bin/voice-io.sh start` from Terminal or your own login item; `--desktop` is Linux-only and there is no signed `.app` bundle. **Actual Mac hardware behavior remains unverified**; check permissions, overlay, hotkeys, and focused paste locally.

## Record

Open Settings with `voice-io.sh settings` or right-click the idle dot at the right screen edge. Choose **Recording mode: Dictation, Meeting, or Collaborative**, save, and restart the daemon. Left-click the dot or use the configured start/finish shortcut to record; click the active pill or use the shortcut again to finish. The toggle and cancel shortcuts provide alternate control. The default shortcuts are `ctrl+alt+space` (start/finish), `ctrl+alt+t` (toggle), and `ctrl+alt+x` (cancel); wake and end phrases are **hey voice** and **voice send**. Settings can change these. In GNOME, enable GNOME shortcuts explicitly in Settings after saving. The wake phrase can start any mode while idle. The end phrase closes and submits only solo dictation; Meeting and Collaborative finish with the shortcut or pill, not the end phrase.

- **Dictation:** Speak English naturally, including words that resemble application commands: they become draft text, not shell/app actions. The offline recognizer finalizes turns, optionally polishes shorter drafts through a configured LLM endpoint, and pastes the finished text once at the focused cursor. No text is typed while speaking.
- **Meeting:** Record and stop to save a private, timestamped, speaker-labelled transcript; nothing is pasted. Optional summary generation sends speaker-labelled transcript chunks to a separately configured OpenAI-compatible chat-completions API. Leave the summary endpoint/model blank to keep the transcript fully offline; a hosted API receives meeting content. Put an API key in the named environment variable of the launching process, never in the config file. An API failure leaves the transcript available without a summary.
- **Collaborative:** Record shared-microphone speakers, then stop to paste one editable, speaker-labelled draft at the focused cursor. It does not auto-submit or request a summary. Speaker labels are acoustic estimates, not verified identities.

Optional local dictation polish uses `[llm] endpoint` (the API root **without** `/v1`) and `model` in `config.toml`; an empty endpoint leaves deterministic offline cleanup only. A non-local endpoint receives the draft text.

Meeting and Collaborative capture **the microphone only**: headphone-only remote participants are absent, and overlapping/similar voices may be mislabelled. Keep the intended destination focused until dictation/collaborative paste finishes. Clipboard/paste delivery is best-effort; on GNOME Wayland without the optional focus bridge the app cannot verify the target text field and uses `Shift+Insert` at current keyboard focus. The bridge reveals focused-window metadata on the user's session D-Bus, not text contents. If paste fails, use `~/.local/bin/voice-io.sh recover` to copy the latest saved draft and paste deliberately. Cancel discards the current recording.

Settings: `$XDG_CONFIG_HOME/voice-io/config.toml` (default `~/.config/voice-io/config.toml`); model and meeting data: `$XDG_DATA_HOME/voice-io` (default `~/.local/share/voice-io`) on Linux, or `~/Library/Application Support/voice-io` on macOS. Logs: `$XDG_STATE_HOME/voice-io/voice-io.log` (default `~/.local/state/voice-io/voice-io.log`) on Linux or `~/Library/Logs/voice-io/voice-io.log` on macOS. `voice-io.sh run` keeps the daemon in the foreground for diagnosis. Run tests from the checkout with `uv run --frozen --with pytest pytest -q` (requires `uv`); model verification is a separate explicit check above.

When publishing to the private team remote, push only the sanitized team branch; do not push all branches or mirror local history. Keep recordings, downloaded models, settings, and logs out of Git.
