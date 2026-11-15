# voicecoder-bridge

Part of [VoiceCoder](https://github.com/TheAtlantaDream/VoiceCoder), a hands-free coding assistant. The bridge is the server between the VoiceCoder web app and the coding agent.

- The web app sends microphone audio or typed text over a WebSocket.
- The bridge turns audio into text with a local Parakeet TDT server (sherpa-onnx int8 CPU), then cleans the text (strips wake phrases like "hey VoiceCoder" / "hey Coder" / "Coder", filler words, and turns "auth dot py" into "auth.py").
- With an agent configured, the bridge sends each cleaned request to the coding agent as a task and returns the agent's summary and the code changes.

Without an agent it only transcribes and cleans.

## Setup

You need Go 1.25 or newer.

```
go build -o bridge ./cmd/bridge
./bridge
```

The bridge listens on `:8000`:

- `ws://localhost:8000/ws` is the WebSocket.
- `http://localhost:8000/health` returns `ok`.

For speech, the bridge needs a Parakeet TDT (sherpa-onnx) server on `http://localhost:8080` that speaks the same `/inference` API as whisper.cpp. Docker Compose builds and starts both:

```
./model-download.sh      # once, downloads Parakeet TDT 0.6B v2 int8 (~0.5 GB)
docker compose up --build
```

Default model is English `sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8`. Override with `PARAKEET_MODEL` (for example the multilingual `sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8`).

The compose file starts the bridge with no agent.

Run the tests with `go test -race ./...`.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `:8000` | Listen address. |
| `--whisper-url` | `http://localhost:8080` | Base URL of the local Whisper STT server. |
| `--silence-rms` | `500` | 16-bit loudness below which audio counts as silence. |
| `--silence-secs` | `1.5` | Seconds of silence after speech that end an utterance. |
| `--end-word` | empty | Optional whole word that ends an utterance as soon as it appears in a partial transcript (for example `over` or `send`). The word and anything after it are dropped from the cleaned request. Empty keeps silence-only ending. `BRIDGE_END_WORD` is used when the flag is empty. |
| `--agent` | `none` | Coding agent: `none`, `fake`, `agentapi`, or `claude`. |
| `--agentapi-url` | `http://localhost:3284` | Base URL of the AgentAPI server. Used with `--agent=agentapi`. |
| `--repo` | `~/project` | Project folder the agent works in. The diff is read from here. Used with `--agent=agentapi` and `--agent=claude`. |
| `--agent-timeout` | `180s` | Longest one request may run. Used with `--agent=agentapi` and `--agent=claude`. |
| `--allowed-origins` | `http://localhost,http://127.0.0.1,https://coder.c1.workshop.cdr.dev` | Comma-separated web origins that may connect when an agent is set. |

### Fake agent

```
./bridge --agent=fake
```

The fake agent changes no code. After one second it returns a fixed result whose summary starts with `[FAKE AGENT]`. Use it to build and test the web app without Claude or an API key.

### AgentAPI

```
./bridge --agent=agentapi --repo ~/project
```

This sends each request to Claude Code through [AgentAPI](https://github.com/coder/agentapi). Start AgentAPI first, inside the project folder:

```
cd ~/project
agentapi server --type=claude -- claude
```

- The project folder must be a git repository (`git init`), or the bridge cannot read the changes.
- `ANTHROPIC_API_KEY` and `ANTHROPIC_MODEL` must be set in the workspace where AgentAPI runs. Never commit them.
- The bridge and AgentAPI must run on the same machine, because the bridge reads the diff from the project folder on disk.

### AgentAPI with Docker Compose

`docker-compose.yml` has an `agent` service that runs AgentAPI, Claude Code and git in one container. It and the bridge share one home volume, so both see the same project folder at `/home/coder/project`.

```
ANTHROPIC_API_KEY=... BRIDGE_AGENT=agentapi docker compose up --build
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `BRIDGE_AGENT` | `none` | Passed to the bridge as `--agent`. Set `agentapi` to use the agent service. |
| `ANTHROPIC_API_KEY` | empty | Key for Claude. Without it the agent service idles. Read it from the environment; never write it into a file in this repository. |
| `ANTHROPIC_MODEL` | empty | Model Claude uses. Empty keeps Claude Code's default. |
| `BRIDGE_ALLOWED_ORIGINS` | empty | Extra origins added to `--allowed-origins`. |
| `ELEVENLABS_API_KEY`, `ELEVENLABS_VOICE_ID`, `ELEVENLABS_MODEL` | empty | Passed to the bridge. See Spoken summaries. |
| `VC_HOME_VOLUME`, `VC_HOME_EXTERNAL` | `voicecoder-agent-home`, `false` | Name of the shared home volume, and whether it already exists. A Coder workspace sets these to its own home volume. |

- **Claude runs with `--dangerously-skip-permissions`.** It edits files and runs commands without asking. It can change anything in the shared home volume and reach the network.
- The agent service creates the project folder and runs `git init` in it when they are missing.
- Both containers run as user id 1000, the same as a Coder workspace.

### Claude directly (fallback)

```
./bridge --agent=claude --repo ~/project
```

Use `--agent=agentapi` normally. This is the fallback for when AgentAPI misbehaves: the bridge runs the `claude` command itself, once per request, and AgentAPI does not need to be running.

- Each request runs `claude -p "<request>" --output-format json` inside `--repo`.
- The `claude` command must be installed on the machine that runs the bridge, with `ANTHROPIC_API_KEY` set there.
- Each request is a new Claude conversation. Claude does not remember earlier requests, but it sees the files and git state they left behind.
- Stop and the timeout end the `claude` process.
- The messages to the web app are the same as with AgentAPI.

## Running without Whisper

Typed requests do not use Whisper. Start the bridge without a Whisper server and send a `text` message:

```
{"type":"text","text":"hey VoiceCoder add a function in auth dot py"}
```

The bridge cleans the text and, with an agent set, runs it. `test-client.html` has a text box that does this.

## Message protocol

Connect to `ws://<host>:8000/ws`.

### Web app to bridge

Text frames are JSON:

| Message | Meaning |
| --- | --- |
| `{"type":"start"}` | Start a spoken request. Audio frames follow. |
| `{"type":"end"}` | The spoken request is finished. The bridge also ends it by itself after `--silence-secs` of silence. |
| `{"type":"stop"}` | Stop listening and cancel a running task. See [Stop](#stop). |
| `{"type":"text","text":"..."}` | A typed request. |
| `{"type":"speech_start"}`, `{"type":"speech_end"}` | The client started or finished playing a `summary` clip. See [Cutting in](#cutting-in). |

Binary frames are audio: 16 kHz, 16-bit, mono PCM.

### Bridge to web app

Every message is JSON with a `type` and a `text`:

| Type | Other fields | Meaning |
| --- | --- | --- |
| `partial` | `seq` | Text of one audio chunk while the user is still talking. `seq` is the chunk number. Not sent from the moment an agent run starts until its `agent_result` (or error) is sent; listening still continues so spoken "stop" works. |
| `transcript` | | The whole spoken request, joined in order. |
| `cleaned` | | The request after cleaning. This is what the agent receives. |
| `status` | | A short line to show or read aloud. See the list below. |
| `agent_partial` | | Live agent reply text while a task runs. Only with an agent that streams. |
| `agent_result` | `diff` | The task finished. `text` is the agent's summary and `diff` is the code changes. Only sent when an agent is set. |
| `speech` | `audio`, `kind` | Text read aloud. `audio` is base64 MP3, `text` is what it says, and `kind` is `intro` (the one-time welcome), `summary`, `narration` (said while the agent works) or `reply` (an answer to the user mid-run). Only sent when ElevenLabs is set up. See Spoken summaries. |
| `interrupt` | none | Stop playing speech now and drop any queued clips. Sent when the user says "hey Coder" or "stop" over a spoken summary, or "hey Coder" during an agent run. |
| `error` | | Something failed. `text` is a plain sentence for the user. |

Example result:

```
{"type":"agent_result","text":"I added add(a, b) to math_utils.py and a test. The test passes.","diff":"diff --git a/math_utils.py b/math_utils.py\n..."}
```

A typed request with an agent set gives, in order: `cleaned`, `status` "Working on it.", zero or more `agent_partial` lines (or progress `status` if none yet), then `agent_result` or `error`.

Spoken requests need a wake phrase somewhere in the utterance: "hey VoiceCoder", "VoiceCoder", "hey Coder", or "Coder". Text before the wake phrase is dropped; everything after it until silence (or an optional `--end-word`) is the request. Without a wake phrase the bridge sends a status reminder and keeps listening. After each spoken utterance it re-arms automatically so the mic can stay open. Typed `text` does not need a wake phrase.

### The `diff` field

- `diff` is only what this request changed: the working tree is snapshotted before the agent runs, then compared afterward. Earlier uncommitted edits from previous requests are left out.
- Ignored files are left out.
- It is empty, and the field is left out of the JSON, when this request changed nothing.
- The same text is also written to `.voicecoder/latest.diff` in the project folder (gitignored) so the web app can open it in VS Code.

### Status messages

| Text | When |
| --- | --- |
| `Listening for hey VoiceCoder…` | The bridge is armed and accepting audio. |
| `Say hey VoiceCoder, hey Coder, or Coder, then your request.` | Speech ended without a wake phrase. |
| `Working on it.` | A task was sent to the agent. |
| `Still working, N seconds so far.` | Every 10 seconds while a task runs and no `agent_partial` has arrived yet. |
| `Still working on your last request. Say stop to cancel it.` | A new request arrived while a task was running. The new request is dropped. |
| `Stopped. Some files may have changed.` | A running task was stopped. |
| `Stopped. Nothing was kept.` | Stop arrived with no task running. |

### Error messages from the agent

| Text | When |
| --- | --- |
| `Stopped.` | The task was cancelled. |
| `That took too long, try a smaller request.` | The task ran longer than `--agent-timeout`. |
| `The agent is still busy. Say stop or wait a moment.` | AgentAPI was already working on something else. |
| `I can't reach the coding agent right now.` | AgentAPI did not answer, or with `--agent=claude` the `claude` command could not be started. |
| `The coding agent did not accept that request.` | AgentAPI refused the message. |
| `The agent finished, but I could not read the changes in the project folder.` | `git diff` failed in `--repo`. |
| `The coding agent ran into a problem and did not finish.` | With `--agent=claude`, the `claude` command failed. |

## Stop

The bridge handles stop itself. Every other request, including "approve", goes to the agent as a normal task.

- A `stop` message cancels the current spoken request and any running task.
- While a task is running, a spoken or typed request that is only "stop" or "cancel" does the same. Case, spaces, and trailing punctuation are ignored, so "Stop." and "stop!" count.
- If a task was running, the bridge interrupts the agent and sends `status` "Stopped. Some files may have changed." An `error` "Stopped." follows when the task has ended.
- A stopped task can leave half-finished edits in the project folder. The user asks the agent in their own words to discard them, or says "approve" to keep them.
- If no task was running, the bridge sends `status` "Stopped. Nothing was kept."
- A new spoken request (`start`) does not cancel a running task.
- One task runs at a time per connection.

A page refresh closes the WebSocket, and closing the WebSocket cancels a running task the same way stop does.

## Origin check

With an agent set, text from the WebSocket becomes a coding task, so the bridge limits which web pages may connect.

- With `--agent=none`, every origin is allowed, as before.
- With an agent, a page may connect only if its origin has the same scheme and host as an entry in `--allowed-origins`. The port must match too, except for `localhost` and `127.0.0.1`, which are allowed on any port.
- A page served from the same host and port it connects to is allowed. This covers the web app when its own server forwards `/ws` to the bridge and keeps the `Host` header.
- An entry may start with `*.` to allow every subdomain: `https://*.example.com` allows `https://app.example.com` but not `https://example.com`.
- A connection with no `Origin` header is allowed. Tools that are not browsers, such as scripts, send none.
- Everything else is refused with HTTP 403. That includes a page opened straight from disk, which sends the origin `null`.

To allow another site, pass the full list:

```
./bridge --agent=agentapi --allowed-origins http://localhost,https://app.example.com
```

The `BRIDGE_ALLOWED_ORIGINS` environment variable adds entries to the list without replacing it.

The bridge logs the chosen agent and the allowed origins at startup.

## Workspace rules file

"Approve" only works if the agent knows what it means. That rule and the others live in `agent-rules.md` in this repository. The agent container copies them into `~/.claude/CLAUDE.md`, Claude Code's user-level rules file, each time it starts:

- The rules are one marked section, from the `# VoiceCoder rules (vN)` heading to `# End VoiceCoder rules`. Anything else in the file is left alone.
- To change the rules, edit `agent-rules.md` and raise the version number in its first line. Containers holding an older section replace it on their next start.
- The rules apply to every project in the workspace, and a project's own `CLAUDE.md` still works alongside them.

The rules name the agent Terrance, make it conversational, and tell it to end each reply with a `Summary:` section, which is the only part read aloud.

### User profile

The agent keeps what it learns about the user in `~/.voicecoder/profile.md`, in the same home volume as the project. The rules import the file (`@~/.voicecoder/profile.md`), so Claude Code loads it at the start of each conversation, and they tell the agent to be curious about the user and add lasting facts and preferences to it. It is kept apart from the rules file because setup rewrites the rules section when the rules change. The file stays on the machine that runs the containers and is never written to a database. Its contents do reach Anthropic as part of the conversation when the agent reads it, like any other file the agent opens, and when the bridge passes it as background to its own short Claude calls (see [Talking while the agent works](#talking-while-the-agent-works)). Delete the file to make the agent forget.

## Spoken summaries

With `ELEVENLABS_API_KEY` set in the bridge's environment, the bridge sends each agent summary to ElevenLabs and passes the audio to the web app:

```
{"type":"speech","kind":"summary","text":"I added add(a, b) to math_utils.py and a test.","audio":"<base64 MP3>"}
```

- It arrives right after `agent_result`, so the summary and diff show without waiting for the audio.
- The agent's status lines, streamed replies, errors, and stopped runs are never read out. Of the agent's own words, only the final summary is spoken.
- The web app plays it, for example `new Audio('data:audio/mpeg;base64,' + audio).play()`. Browsers only allow that after the user has clicked or typed on the page once.
- If ElevenLabs fails, the bridge logs the reason and sends no `speech` message. The result is unaffected.
- Only the closing summary is spoken. The bridge reads aloud what follows the last `Summary:` line of the agent's reply (`**Summary:**` and `## Summary` also count). The full reply still goes to the screen in `agent_result`.
- A reply with no `Summary:` line is spoken with its code blocks removed, so code is never read out by accident. Code inside the summary is spoken, because the agent only puts it there when the user asks to hear it.
- Markdown symbols are dropped from the spoken text.
- The first request sent to the agent is followed by a one-time welcome `speech` message, sent before the agent finishes. It is remembered in `.voicecoder/intro-played` next to the project folder, so restarts do not repeat it. A client must play `speech` messages in order, not on top of each other.
- Summaries longer than 1200 characters are cut before they are spoken, because ElevenLabs bills per character.

### Cutting in

The user can interrupt by voice at two points. Both need "hey Coder" or "hey VoiceCoder"; a bare "Coder" is not enough, so a passing mention does not cancel work.

- **While the agent is working.** As soon as the phrase is heard the client gets `interrupt` so it stops talking. With talk on (below), the run carries on and the rest of the utterance is answered as a side conversation. With talk off, the run is stopped and the rest of the utterance (ended by silence or the end word, as usual) is sent as the next request once the stopped run has settled.
- **While a summary is being spoken.** The client must keep sending microphone audio while a `summary` clip plays, and tell the bridge with `{"type":"speech_start"}` and `{"type":"speech_end"}`. In between, other speech is ignored (it may be the app's own voice picked up by the mic), "stop" sends `interrupt`, and "hey Coder" sends `interrupt` and starts a new request.

A client should not send microphone audio while the `intro` clip plays: the welcome itself says "hey Coder" and "over".

### Talking while the agent works

With `ANTHROPIC_API_KEY` in the bridge's environment and an agent set, the bridge makes short Claude calls of its own, separate from the coding agent's session:

- **Narration.** A few seconds into a run, and then every few seconds while the agent's output keeps changing, the bridge asks for one plain, conversational sentence about what is going on and sends it as a `status` message and a `narration` `speech` message. The agent's raw output is given to the model as notes only; the prompt forbids reading out code, commands, paths or file contents, and code blocks are stripped from the answer before it is spoken. A line that is ready only after the run ended is dropped. Needs ElevenLabs.
- **Side conversation.** Something said with "hey Coder" (or typed) during a run is answered without stopping it, as a `status` message and a `reply` `speech` message. The model also picks one action: nothing, stop the run (when the user clearly asks for that), or queue the request (when it is new coding work) to start when the current run ends. The `stop` control drops a queued request. The side voice cannot change project files. When the user tells it something lasting about themselves, it adds a line to the profile file, which the coding agent also reads.
- Each narration line and each answer is one extra Claude request on the same key. If a call fails, narration is skipped and a side question gets the plain "Still working" status.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANTHROPIC_API_KEY` | empty | Turns talk on. Read from the environment only. |
| `BRIDGE_TALK` | on | `off` turns narration and side conversation off. |
| `BRIDGE_TALK_MODEL` | `ANTHROPIC_MODEL`, else `claude-haiku-4-5` | Model for these calls. |
| `BRIDGE_PROFILE_FILE` | `<project>/../.voicecoder/profile.md` | The user's profile, passed as background. |
| `ELEVENLABS_API_KEY` | empty | ElevenLabs key. Empty turns speech off. Read from the environment only; never write it into a file in this repository. |
| `ELEVENLABS_VOICE_ID` | `JBFqnCBsd6RMkjVDRZzb` | Voice to use. |
| `ELEVENLABS_MODEL` | `eleven_flash_v2_5` | ElevenLabs model. Flash is their low-latency model. |
| `BRIDGE_INTRO` | built-in welcome | Text of the one-time welcome. `off` turns it off. |
| `BRIDGE_INTRO_FILE` | `<project>/../.voicecoder/intro-played` | File that records the welcome was played. |

## Test page

`test-client.html` connects to `ws://localhost:8000/ws`. It can record from the microphone or send typed text, and it shows every message, including the `agent_result` summary and diff, and plays `speech` audio.
