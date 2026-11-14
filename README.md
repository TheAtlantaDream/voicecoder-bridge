# voicecoder-bridge

The bridge is the server between the VoiceCoder web app and the coding agent.

- The web app sends microphone audio or typed text over a WebSocket.
- The bridge turns audio into text with a local whisper.cpp server, then cleans the text (removes the "hey VoiceCoder" wake word and filler words, and turns "auth dot py" into "auth.py").
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

For speech, the bridge needs a whisper.cpp server on `http://localhost:8080`. Docker Compose builds and starts both:

```
./model-download.sh      # once, downloads the small.en model
docker compose up --build
```

The compose file starts the bridge with no agent.

Run the tests with `go test -race ./...`.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `:8000` | Listen address. |
| `--whisper-url` | `http://localhost:8080` | Base URL of the whisper.cpp server. |
| `--silence-rms` | `500` | 16-bit loudness below which audio counts as silence. |
| `--silence-secs` | `1.5` | Seconds of silence after speech that end an utterance. |
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

Typed requests do not use Whisper. Start the bridge without a whisper.cpp server and send a `text` message:

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

Binary frames are audio: 16 kHz, 16-bit, mono PCM.

### Bridge to web app

Every message is JSON with a `type` and a `text`:

| Type | Other fields | Meaning |
| --- | --- | --- |
| `partial` | `seq` | Text of one audio chunk while the user is still talking. `seq` is the chunk number. |
| `transcript` | | The whole spoken request, joined in order. |
| `cleaned` | | The request after cleaning. This is what the agent receives. |
| `status` | | A short line to show or read aloud. See the list below. |
| `agent_result` | `diff` | The task finished. `text` is the agent's summary and `diff` is the code changes. Only sent when an agent is set. |
| `error` | | Something failed. `text` is a plain sentence for the user. |

Example result:

```
{"type":"agent_result","text":"I added add(a, b) to math_utils.py and a test. The test passes.","diff":"diff --git a/math_utils.py b/math_utils.py\n..."}
```

A typed request with an agent set gives, in order: `cleaned`, `status` "Working on it.", zero or more progress `status` lines, then `agent_result` or `error`.

### The `diff` field

- `diff` is everything in the project folder that is not yet approved (committed): staged and unstaged edits, and new files. Ignored files are left out.
- It is not only the last request. If two requests ran and neither was approved, the diff shows both.
- It is empty, and the field is left out of the JSON, when nothing is waiting for approval. That is the case right after an approve.

### Status messages

| Text | When |
| --- | --- |
| `Working on it.` | A task was sent to the agent. |
| `Still working, N seconds so far.` | Every 10 seconds while a task runs. |
| `Still working on your last request. Say stop to cancel it.` | A new request arrived while a task was running. The new request is dropped. |
| `Stopped. Some files may have changed. Say reject to throw them away, or approve to keep them.` | A running task was stopped. |
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

The bridge handles stop itself. Every other request, including "approve", "reject", and "undo", goes to the agent as a normal task.

- A `stop` message cancels the current spoken request and any running task.
- While a task is running, a spoken or typed request that is only "stop" or "cancel" does the same. Case, spaces, and trailing punctuation are ignored, so "Stop." and "stop!" count.
- If a task was running, the bridge interrupts the agent and sends `status` "Stopped. Some files may have changed. Say reject to throw them away, or approve to keep them." An `error` "Stopped." follows when the task has ended.
- A stopped task can leave half-finished edits in the project folder. The user says "reject" to discard them or "approve" to keep them.
- If no task was running, the bridge sends `status` "Stopped. Nothing was kept."
- A new spoken request (`start`) does not cancel a running task.
- One task runs at a time per connection.

A page refresh closes the WebSocket, and closing the WebSocket cancels a running task the same way stop does.

## Origin check

With an agent set, text from the WebSocket becomes a coding task, so the bridge limits which web pages may connect.

- With `--agent=none`, every origin is allowed, as before.
- With an agent, a page may connect only if its origin has the same scheme and host as an entry in `--allowed-origins`. The port must match too, except for `localhost` and `127.0.0.1`, which are allowed on any port.
- A connection with no `Origin` header is allowed. Tools that are not browsers, such as scripts, send none.
- Everything else is refused with HTTP 403. That includes a page opened straight from disk, which sends the origin `null`.

To allow another site, pass the full list:

```
./bridge --agent=agentapi --allowed-origins http://localhost,https://app.example.com
```

The bridge logs the chosen agent and the allowed origins at startup.

## Workspace rules file

Approve, reject, and undo only work if the agent knows what they mean. Those rules live in `~/.claude/CLAUDE.md`, Claude Code's user-level rules file. It applies to every project in the workspace, and a project's own `CLAUDE.md` still works alongside it.

If the file already exists, add the rules under their heading instead of overwriting it.

```
# VoiceCoder rules

- Only edit files inside the project folder you were started in. Never run git push.
- After any code change, run the project's tests if it has any.
- Only commit when the user says "approve": run git add -A and git commit with a short message describing the change.
- "Reject" means discard every uncommitted change, including new files: git checkout . and git clean -fd.
- "Undo" means revert the last commit: git revert --no-edit HEAD.
- Never commit unless the user said approve.
- End every reply with a plain-language summary of at most three sentences: which files changed, what the change does, and the test result if tests ran. This summary is read aloud to the user.
```

The last rule matters to the bridge: the agent's final reply is what `agent_result` returns as `text`.

## Test page

`test-client.html` connects to `ws://localhost:8000/ws`. It can record from the microphone or send typed text, and it shows every message, including the `agent_result` summary and diff.
