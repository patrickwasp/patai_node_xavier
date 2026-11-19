# Apps

What an owner turns on is an app, made of services. A service is a Compose recipe in `workloads/` with a
`manifest.json` that pins its image and models; an app is a manifest in `apps/` naming its APIs and the services
behind them. Two apps can share a service, which runs once. The recipes and manifests ship in the signed OS, and
`patai-fetch` checks everything they pin, so the OS signature vouches for what runs. Nothing is built on a node.

## What an app declares

`apps/<app>/manifest.json`:

| Field | What |
| --- | --- |
| `name`, `title`, `about` | The app, as the console shows it |
| `apis` | Each API: `name`, `title`, `about`, the `service` behind it, the `routes` the gateway sends it, and whether it's `always` on or on by `default`. `with` names services it runs beside its own, such as the agentic API's SearXNG |
| `services` | Services it runs whatever its APIs |
| `key` | For an app that reaches its APIs with a key of its own: the variable its `services` get it as. Its APIs are then its own, reached only through the gateway for apps, and owners' keys can't use them |
| `secrets` | Secrets the node makes for the app, passed to its `services` by variable, such as its database's password |
| `ui` | Its own web pages: the `port` the console serves them on, from one of its `service`s, and `routes` sending the paths under a prefix to another, such as `/api/` to its server, so a browser sees one origin. People sign in with the app's own accounts |

`workloads/<service>/manifest.json`:

| Field | What |
| --- | --- |
| `name`, `title`, `version`, `licences` | The service, and the licences of what it runs |
| `gpu`, `ready`, `networks` | Whether it uses the GPU, the loopback address of its ready check, or `healthcheck` for one with no HTTP, whose recipe has a health check Docker runs, and Docker networks it joins |
| `image` | The image on GHCR: `ref`, manifest `digest`, Docker `id`, its `layers` (digest and size) with their total `size`, and its `unpacked` size |
| `models` | Pinned models: `name`, `title`, whether `default` (kept unless the owner says otherwise), `dir`, `sha256`, `size`, `url`, and the `mirror` that has a copy in pieces. `files` are more a model needs beside it, such as a vision projector or an MTP draft, kept and removed with it. `env` is what it needs over the profile, `settings` its defaults, `limits` its bounds, and `memory` its measured peak: `base_mib`, `kv_mib_per_1k` and `request_mib`, what each request at once keeps of its checkpoints |
| `model` | How it takes models: `env` names the setting a one-at-a-time service reads its model from, `dir` where owners' own go, and `formats` what they can be (`gguf`, `safetensors`) |
| `profiles`, `profile` | Settings sets with what each takes in memory, and the default one |
| `memory` | For a service whose peak goes by a setting rather than a model, such as the image engine by largest size |
| `settings` | What an owner can change: a name, title, type (`int`, `number`, `choice`, `bool`), range or choices, default, and the variable it sets, or `for: gateway` |
| `secrets` | Variables the owner gives, such as a search provider's key, `made` ones the service makes for itself, or ones given `once`, such as the password of an account it makes at its first start |

`console/internal/workloads/manifest_test.go` holds every shipped manifest to what `patai-fetch` and `patai-workload`
take: hashes, sizes, names a file can have, a gate in every recipe, and every setting reaching the server.

## Running them

`patai-workload` serves the apps that are on, with nobody logged in:

```
patai-workload hold             before Docker starts: no restarting by itself for a container without its gate
patai-workload serve            run every service the apps that are on need, at every boot
patai-workload start|stop <app> turn an app on or off
patai-workload api on|off <app> <api>
patai-workload show             the desired state, and each service's models and settings, as JSON
patai-workload set <service> <json>      change a service's settings for the model it runs
patai-workload model use|get|remove <service> <model>
patai-workload model add <service> <name> <url>   download an owner's own model
```

- **The desired state** is `/persist/provision/apps.json`: the apps that are on, each one's APIs, and each service's
  settings. Provisioning may write it with the first app to turn on. An app or service this release doesn't have stays
  stopped, with its data kept.
- **Compose runs on the release's own recipe** in `/usr/share/patai/workloads`, with nothing from `/data`: an override
  or `.env` file left there can't change what starts.
- **The gate.** Docker brings a `restart: on-failure` container back by itself after a power cut, before anything has
  checked it. So each server mounts `/run/patai/apps/<service>`, which only `patai-workload` makes, and `/run` is empty
  after every boot. A container is made, checked for its gate and pinned image, and only then started. `hold` turns
  off the restart policy of any container without its gate before Docker starts. Keep the gate in every new recipe.
- **`patai-fetch`** gets what a manifest pins, with no update channel needed: the image from GHCR by digest, which
  Docker checks layer by layer, then its ID; each model from its `mirror` if it has one, otherwise from its `url`, in
  64 MiB ranges, six at a time. A download resumes from `<file>.progress`, is checked against its hash in a folder no
  container can see, and only then moves into place. A model's marker records the hash and the file it was true of,
  so a checked model isn't hashed again at each boot. Progress is in `/run/patai/fetch.json`.
- **Owners' own models** can be GGUF or safetensors only, checked by their headers, which hold weights and can't run
  code. A pickle checkpoint never. Services mount `/data/models` read-only.
- **Secrets** are in `/persist/provision/secrets.json`, which only root reads, and reach a container as the variables
  its manifest names. `show` says which are set, never what they are. One given `once` is passed until the service
  has answered its ready check, then removed, and the container made again without it: Docker keeps a container's
  environment where root can read it.
- **An app with a key of its own** gets it made like a secret. Its `services` get the key, its made secrets and its
  APIs that are on, as `PATAI_APIS`. `show` gives the key's SHA-256, which the gateway for apps checks.
- **By hand,** `scripts/workload <service> <compose args>` copies `workloads/` to a node and runs Compose there; starting
  a server this way opens its gate.

## Images and models on GitHub

- **Images** are `ghcr.io/patrickwasp/patai-<service>`. The GPU ones are built in NVIDIA's CUDA devel image and copied
  onto `patai-cuda-base` (`workloads/cuda-base`): Ubuntu 20.04 with only the CUDA runtime, cuBLAS and OpenMP, 610 MB
  unpacked and 270 MB to download. NVIDIA's runtime mounts the driver from the OS, but only into a container with
  `NVIDIA_VISIBLE_DEVICES`, which the base sets. Layers are zstd, and the base's are downloaded once: llama.cpp adds
  43 MB. Images are built on a node, and `scripts/publish-workloads [service...]` copies each from it, pushes it, and
  pins it in every manifest that runs it; for `cuda-base` it pins the base in each Dockerfile instead. It refuses an
  image labelled `patai.test`.
- **Chat's image** is built in [patai_chat](https://github.com/patrickwasp/patai_chat) on the PC, for arm64,
  by its `scripts/build-image`, and loaded on a node to publish it the same way:
  `docker save ghcr.io/patrickwasp/patai-chat:<version> | scripts/ssh docker load`, then
  `scripts/publish-workloads chat-server`, which pins it in `chat-web` too.
- **GHCR packages are private when first pushed,** and only the package's settings page can make them public. Make
  each new one public and link it to this repo, or nodes can't pull it.
- **Models** are mirrored in the release `models` as 1 GiB pieces, `<name>.000` on, by the Mirror models workflow
  (`scripts/mirror-models`), which streams each from its `url` on GitHub's side, checks it against its pin, and takes
  the pieces down if it doesn't match. Run it after pinning a model with a `mirror`. A model whose licence doesn't
  allow a copy, such as Liquid AI's, has no `mirror` and comes from its own address.

**Easy to get wrong:**

- Workload ports are on loopback. From the network a service is reached only through the console's gateway.
- A container's memory limit doesn't cap CUDA memory: nvmap allocates it outside the cgroup. Freed GPU memory stays in
  nvmap's pool, up to 3.8 GiB, which Linux counts as used.
- An app's image carries Compose's project label, so a container run from it by hand inherits it: `serve` counts only
  what Compose made (`com.docker.compose.oneoff=False`).
- Docker's default seccomp refuses calls focal's libseccomp doesn't know, `close_range` among them, with EPERM rather
  than ENOSYS, so a program that falls back only on ENOSYS fails. Such a container needs a profile answering ENOSYS.
- An image's health check is lost in OCI's format, so a recipe that needs one, such as Postgres's, has it in its
  `compose.yaml`. An `ENTRYPOINT` in a Dockerfile clears the command the base image had.

## The AI API

`apps/ai-api`: OpenAI-compatible APIs for other programs, with keys, limits and usage.

| API | Service | Routes | Default |
| --- | --- | --- | --- |
| Chat | `llama-cpp`: llama.cpp v1.0.0 | `/v1/chat/completions`, `/v1/completions`, `/v1/responses` | Always on |
| Embeddings | `llama-embed`: llama.cpp and nomic-embed-text v1.5 | `/v1/embeddings` | On |
| Images | `sd-cpp`: stable-diffusion.cpp and FLUX.2 klein 4B | `/v1/images/generations` | Off |
| Speech to text | `speech-to-text`: Cactus 4.1.0 and Whistle, or whisper.cpp v1.9.5 and Whisper | `/v1/audio/transcriptions` | Off |
| Text to speech | `piper`: Piper 1.8.0 | `/v1/audio/speech` | Off |
| Agentic API | `agentic-api` with `searxng` | `/v1/responses`, `/v1/conversations`, `/v1/messages` | On |

**Chat's default model** is Gemma 4 26B-A4B QAT, which sees images through its projector and writes faster with its
MTP draft. The other pinned models are an owner's choice, as are their own GGUF files. The smaller Gemma 4s, 12B, E4B
and E2B, have MTP drafts too, guessing 2 tokens ahead: one conversation writes 1.8, 1.5 and 1.24 times as fast. Each
pin has its own defaults, such as thinking off for Gemma 4 and Qwen3.6.

### The gateway

The console's gateway is the only way in to the services. It routes by path, to the service behind the API that has
it, while that API and its app are on; otherwise it answers 404 in OpenAI's shape. `/v1/models` lists what the APIs
that are on run, and only those a key may use, each with what its service says it takes and gives, as llama.cpp's
`architecture`: `{"input_modalities": ["text", "image"]}` for a chat model that sees. A model's name isn't routed on:
each service runs one model.

- **On 443** it takes owners' keys and the sessions of people who can run apps, for the APIs of apps without a key of
  their own.
- **The gateway for apps**, plain HTTP on 18000, takes only an app's own key, as a bearer token. `patai-firewall` lets
  only containers' bridges and loopback reach it, and a container reaches it as `host-gateway`. An app gets its own APIs that are on, logged under its name;
  `/v1/models` lists only what it chats with. Search results and audio use no tokens.
- **`/v1/audio/voices`** lists the voices of the text-to-speech API that's on, from the recipe, its default first.
  OpenAI has no such path.

- **Usage** is counted on the node, per key and per day, from each answer's `usage`, or from llama.cpp's `timings`
  in a stream that didn't ask for it, and images by count. Never prompts or outputs.
- **The request log** keeps a line per request: time, key, API, path, model, status, tokens, images and time taken.
  Kept 30 days and 60,000 lines.
- **GPU time is shared, not divided.** Chat goes first. Images go one at a time, with a short queue that answers 429
  when it's full, and a request whose client has gone is cancelled.

### Keys

`patai_` keys, each limited to some of the app's APIs, with requests at once, request size, tokens a day, images a
day and an expiry. Kept hashed: a key is a token with the `key` scope, and its limits a row of `key_limits`. The
gateway strips a key before passing a request on. A day is UTC, and what a request uses is known only once it's
answered, so one request can take a key past its limit, and the next is refused.

### Images

`POST /v1/images/generations`, in OpenAI's shape, made by stable-diffusion.cpp's `sd-server`. Steps and count are
bounded, and images come back as `b64_json` only: the node keeps none. A model picks its own size, so a size the
settings don't allow is made as near as they do, its shape first, and the answer's `size` says which. Each largest
size makes a portrait and a landscape of no more pixels than its square, such as 448×576 at 512.

**Beside a large chat model** Images doesn't fit: with Gemma 4 26B and the other apps on, 1.0 GiB of the 26 is left.
Measured on patai-dev 2026-11-17, one model at a time with the GPU's pool already filled by earlier work, so lower
than the figures above: FLUX.2 klein Q8_0 peaked at 7.0, 9.6 and 12.7 GiB at 512, 768 and 1024, and unsloth's Q3_K_M
at 6.3, 7.7 and 10.5, at the same speed (15, 30 and 58 s) and much the same pictures. Measured again from an empty pool,
with the apps that use the chat model stopped: Q8_0 with its Qwen3 4B text encoder at Q8_0 peaked at 8.3, 10.7 and 14.1
GiB; Qwen's own Q4_K_M of the encoder saves 1.3 GiB at each size, and with Q3_K_M 3.3, at 5.0, 7.4 and 10.9, as fast,
some pictures laid out a little differently.

### Voice

- **Speech to text** is `patai-transcribe`, the recipe's own, at OpenAI's path: a recording as the form's `file`, and
  `language` and `response_format`. It runs the model the owner chose. For Whistle it hands each recording to Cactus's
  engine on the CPU, and answers `json` or `text`. For Whisper it becomes whisper.cpp's server on the GPU, which takes
  `prompt` and `temperature` too, and answers `srt`, `vtt` and `verbose_json` as well. The language is found in each
  recording unless a request names it. One recording at a time. Its image has an FFmpeg of its own, 2.6 MB, that only
  turns a recording into WAV: Opus in WebM or Ogg, AAC in MP4, MP3, FLAC and WAV, which is what browsers record.
- **Its models:** Cactus's Whistle by default, 16.9 MB, in English, German, French, Spanish, Italian, Dutch and Polish:
  another language named is found as if none were. Cactus's engine is their own build of it for Linux on ARM64, checked
  against their release's manifest. Whisper small at 8 bits and large-v3 turbo at 5 bits are choices, in 99 languages
  and better with names and numbers. With CUDA each takes more than its file, 1.26 and 1.58 GiB: beside the 26B and Chat,
  small fits with 62 MiB to spare, and turbo doesn't. On the CPU alone small took 15 to 25 s for an 11 s recording, so it
  stays on the GPU.
- **Text to speech** is Piper on 4 of the CPU's cores, behind `patai-speech`, the recipe's own, as Piper's server can
  download voices: `input` of up to 4,096 characters, `voice` and `speed`. It answers in WAV only, as the image has no
  encoder; a request for another format is refused. A voice it doesn't have, such as OpenAI's `alloy`, gets the
  default. It runs as root in its container, with every capability taken away: `patai-fetch` keeps models as root's
  alone. Its image sets `ORT_DISABLE_TELEMETRY=1`: ONNX Runtime 1.32 otherwise sends Microsoft telemetry from Linux
  too, which it did on patai-dev.
- **Its voices** are Piper's medium ones whose recordings are public domain or CC0, and Cori's, CC BY 4.0: LJ Speech and
  John kept by default, Kristin, Norman, Bryce, Joe and Cori as choices. Lessac, Amy, Ryan and the HFC voices aren't
  pinned, as their recordings' licences don't allow commercial use or are unclear. The high-quality voices are too slow
  for the Carmel cores.
- **Measured on patai-dev**, beside the 26B: an 11 s recording in every format transcribed in 0.8 s with small and
  2.1 s with large-v3 turbo, word for word. Whistle took 0.5 to 1 s for a question of a few seconds and 42 s for 10
  minutes, with 14 MiB at rest and 220 at its peak, the longest recording's copies on `/tmp` counted. It heard
  Kennedy's "ask not" word for word, but "light ask keeper" and "too plus too" in Piper's "lighthouse keeper" and "two
  plus two", which Whisper small heard right. Medium voices spoke 5 to 6 times as fast as they're heard, high ones at
  about 1. Images of 305 MB to download (speech to text), most of it the CUDA base the GPU services share, and 99 MB
  (Piper).

### The agentic API

[agentic-api](https://github.com/vllm-project/agentic-api) (Apache-2.0) is a second gateway in front of the model. It
runs tools itself, such as web search through SearXNG on the node or a provider with the owner's key, keeps
conversations on the node, and answers Anthropic's `/v1/messages` beside OpenAI's. `/v1/chat/completions` still goes
straight to llama.cpp.

- It reaches chat and SearXNG on a Docker network of their own, `patai-ai`, so a request naming an MCP server on
  loopback reaches only agentic-api's own.
- Usage is read only where an answer puts it, so a request can't set its own count. A WebSocket is refused, since its
  frames would pass uncounted.
- Stored responses and conversations aren't tied to the key that made them: any key that knows an id can use one.
- It runs the Python a model writes in its code interpreter (below).

### The code interpreter

The agentic API runs Python a model writes when a request gives it the tool, as OpenAI's Responses does:
`{"type": "code_interpreter", "container": {"type": "auto"}}`. It's agentic-api's own, in
[Eryx](https://github.com/eryx-org/eryx) 0.8.0: CPython compiled to WebAssembly, run by Wasmtime 48.0.9. The model
sees a function, `code_interpreter(code)`, and the answer has a `code_interpreter_call` with the code, its `status` and
what it printed as `logs`. Research and what was rejected: `research/code-interpreter.md`.

- **What it gives:** stdout and stderr. No files or images, Python's standard library only, and a fresh interpreter
  for every call, so nothing carries from one to the next. The tool's other fields, `file_ids` and `memory_limit`
  among them, are refused.
- **The sandbox is WebAssembly.** The guest has no system calls, only what Eryx gives it: the standard library,
  read-only, as `/python-stdlib`, and two output streams. No network, not even loopback, no file it can write, no
  processes, no environment but Python's paths, no host calls. agentic-api builds the sandbox without Eryx's
  networking or its in-memory files.
- **Each call is a worker process of its own**, started with nothing in its environment and its output on
  `/dev/null`, talking to the server over a private socket that only it may connect to. It's killed at its deadline,
  when the request is cancelled or its client goes, and the next call gets a new one.
- **Its own cgroup** holds each worker to its limits, below the container's. The server makes them as the container's
  user, which can't change the container's own limits (Running it).
- **Owners turn it off** with the agentic API's Code interpreter setting, on by default. A request that gives the
  tool while it's off is refused before the model runs.

| Each run | Limit | Measured |
| --- | --- | --- |
| Memory | ⟨⟩ for the guest, Wasmtime's; ⟨⟩ for the worker, its cgroup's, with no swap | ⟨⟩ |
| CPU | One core, `cpu.max` | ⟨⟩ |
| Processes | 64, `pids.max`; a guest can't make one | ⟨⟩ |
| Time | ⟨⟩ s, and ⟨⟩ of Wasmtime's fuel | ⟨⟩ |
| Output | 64 KiB of stdout and 64 KiB of stderr kept; more stops the run | |
| Code | 64 KiB | |
| Disk | None: nothing is writable | |
| At once | ⟨⟩; another call at the same time fails at once | |

#### Running it

Docker mounts a container's cgroup read-only, so the server couldn't make a worker's. The recipe runs on Docker's
runtime `patai-delegate`, `/usr/local/sbin/patai-runc`, which is runc with two things more when it creates the
container:

- the container's `/sys/fs/cgroup` is writable. In its own cgroup namespace that's the container's cgroup and what's
  below it, nothing else.
- `delegate/`, below it, is the container's user's, as systemd delegates one: the folder and its `cgroup.procs`. The
  container's processes are in `delegate/main`, and the server makes each worker's cgroup beside it, with the memory,
  pids and cpu controllers on. The limits of `delegate/` and of the container stay root's.

That's safe only while nothing in the container can be root: the node's cgroup2 has no `nsdelegate`, so only file
ownership keeps a container from raising its own limits. `patai-runc` refuses a container that runs as root or in
root's group, has a capability, may gain privileges, or shares the host's cgroup namespace. The recipe runs as 10001
with every capability dropped, `no-new-privileges` and a read-only root. Its `/tmp` is a 192 MiB tmpfs, gone when it
stops, where the first worker unpacks Eryx's runtime and the standard library for the others to map. The container
is held to ⟨⟩ and 256 processes. `patai-rootd` reads its memory from its scope, workers included.

The image is built on a PC for arm64 (`workloads/agentic-api/Dockerfile`), from agentic-api v0.10.0 with
`patai.patch`:

- Wasmtime 48.0.9 for Cranelift's fixes, one of them for aarch64 (GHSA-mghf-wjfp-p5xm, critical);
- a worker killed without `cgroup.kill`, which Linux has from 5.14;
- one core per run;
- a precompiler that compiles Eryx's runtime, pinned by its hash, for the Xavier with the server's own Eryx and
  Wasmtime.

#### Measured

⟨⟩

#### Decisions for Patrick

⟨⟩

### Settings

Each setting has a default, and Reset puts them all back. A change restarts only the service it belongs to.

| Chat | Default | Choices |
| --- | --- | --- |
| Context | 64k, 32k for each of 2 at once | 4k to 128k, up to the model's limit, shared by the requests at once |
| Requests at once | 2 | 1 to 4 |
| Thinking | The model's pin | Auto, on or off; effort for gpt-oss |
| Temperature, top-p, top-k, min-p, longest answer | The model's own | For requests that don't set their own |
| 8-bit cache | Off | Halves the cache's memory |

| Images | Default | Choices |
| --- | --- | --- |
| Largest size | 512×512 | 512, 768 or 1024 on a side, or as many pixels in portrait or landscape |
| Steps, guidance, sampler | The model's | For requests that don't set their own |
| Waiting | 2 | Images queued before the next is refused |

llama-server's cache of past prompts is capped at 2 GiB, not a setting: without the cap a node ran out of memory and
hung. Each request keeps up to 6 context checkpoints, 32k apart: a model with sliding-window layers, such as Gemma 4,
needs one to take up a prompt it shares only the start of, as a new conversation does its system prompt, and at
llama-server's own 32 a long conversation held 6.7 GiB. A request goes to the slot holding at least 95% of its prompt,
or else to the one used least lately. An app's sessions at once are its requests at once: each needs its own cache for
its whole context, so it's the memory that decides how many, and with Gemma 4 26B and both apps on that's 2 of 64k.

## The Chat app

`apps/chat`: [patai_chat](https://github.com/patrickwasp/patai_chat), patai's own chat app, for each person with an
account, on 8443. One image, `patai-chat`, holds both of its services: `chat-server`, the API at `/api/v1` with
people, sessions, chats and answers, and `chat-web`, the web app's pages. `chat-postgres` is Postgres 18 with
pgvector 0.8.7, `patai-postgres`, with a database of its own. Open WebUI was the Chat app until 2026-11-17.

| Uses | Service | Default |
| --- | --- | --- |
| Chat | `llama-cpp`, the AI API's | Always on |
| Documents | `llama-embed` | On |
| Web search | `searxng`, through the gateway at `/search` | On |
| Images | `sd-cpp` | Off |
| Speech to text | `speech-to-text` | On |
| Text to speech | `piper` | On |
| Computer | `open-terminal`, a container for each person (The terminal) | Off |

Its services run once if the AI API is on too, but its APIs are its own: owners' keys reach the AI API's, and the
app's key reaches only these, through the gateway for apps.

- **One origin.** The console serves the pages from `chat-web` and sends `/api/` to `chat-server`, so sign-in cookies
  are first-party and the event stream and WebSockets pass through. On 8443, and at the unit's address when the owner
  puts Chat in front (`console.md`, The front page); owners then find the console on 9443, linked from Chat's menu. The server takes the name the console was reached
  by, which the console has checked, and its port is on loopback.
- **The owner's account** is a username and a password, asked for when the app is first turned on and given once. The
  owner adds everyone else in the app.
- **Isolation:** each runs as a user of its own on a read-only image. `chat-server` and `chat-postgres` share the
  network `patai-chat`, and `chat-web` has only its own: the browser calls the API, not the pages' server.
- **Photos** people send are kept in `/data/chat-server`, each with copies the server makes for the model and the
  chat, and go with their chat. Photos are offered while the chat model sees.
- **Its memory**, measured on patai-dev 2026-11-17 with 0.2.0: the server 142 MiB at rest, 143 at its peak taking a
  48 MP JPEG, 188 for a 50 MB PNG, and 313 for a 1,000-page PDF, its heap growing while nothing held it back. So its
  heap is held to 192 MiB, which took the PDF to 269, and it counts 384. A recipe's memory is what the console counts
  to admit apps; no container has a limit.
- **Images** come from the AI API's `sd-cpp` while its Images API is on, off by default as it doesn't fit beside the
  default chat model. The app keeps each one made as it does a photo.
- **Documents** people send are read into its Postgres, which has pgvector, and long ones found by meaning with
  `llama-embed` while its Documents API is on, and by their words without it.
- **Voice** comes from the AI API's `speech-to-text` and `piper` while its Speech to text and Text to speech APIs are
  on, both on by default, as with Whistle they fit beside the 26B: dictation and voice conversations need the first,
  and voice conversations the second too. The browser records 16 kHz WAV itself and the server passes each recording
  and sentence on, keeping neither. With Text to speech off, answers are read in the browser's own voices that run on
  the device.
- **It reaches** the model, embeddings, web search, voice and each person's computer through the gateway for apps, with
  the app's own key, and reads the pages a search finds itself, refusing any address that isn't on the public internet:
  the node, the LAN and the gateway included.
- **The computer:** while its API is on, patai runs commands and reads, writes, lists and shows files in the person's
  own container, each chat in a folder of its own under `chats/`, and the side panel has their files, what patai ran,
  and a terminal they type in, which the server passes through as a WebSocket. Each command asks first, as does
  writing over a file, until the person allows commands for the chat; deleting for good, and a command after reading a
  web page, always ask. Code in an answer can be run there with Run. A file is served to the browser as a download,
  unless it's a photo.
- **Connectors:** each person can add MCP servers by their address in Settings, signing in at the server's own page
  (OAuth), and patai uses their tools, asking first as the person chose for each. Only MCP's stateless version,
  2026-07-28. The server reaches a connector itself, over the `patai-chat` network's way out: https on the internet,
  or http and https on the LAN's 10.0.0.0/8 and 192.168.0.0/16, never the node, Docker's 172.16.0.0/12 or link-local.
  One with a sign-in has to be https, as OAuth's credentials go over https only.
  What a sign-in gives is kept in the app's database, encrypted with a key from its `BETTER_AUTH_SECRET`.
- **Measured on patai-dev** (2026.11.16-1, beside Open WebUI with voice off): an image of 73 MB to pull; 145 MiB at
  rest and 147 while answering (server 66, web app 51, Postgres 29), against Open WebUI's 404; first words 1.7 s after
  sending. After a power cut mid-answer it answered again 79 s after power came back, by itself, and the answer was
  marked interrupted with all but its last second kept.

## Models from services

Chat models of outside services, which an owner adds on the Apps page: OpenRouter, OpenAI, Anthropic, or any other that
answers as OpenAI's does, at an https address or an http one on the LAN. Each is added with an API key of the owner's,
which stays in the console's `/persist/console/outside.json`, readable by the console alone, and is never shown again
or given to an app. The owner chooses which of a service's models to offer, from the list the service gives, and which
apps may use them: none until they choose, and **Owners' API keys** for the AI API on 443.

- **Choose models** is a table of the service's models (`catalogue.go`): fit, intelligence, maker, what each takes,
  its effort levels and default, context and price, with a checkbox to offer each. It opens on those that fit. A model
  fits when it calls tools and writes text, since Chat's search, memory and documents are tools. Each chosen model is
  kept as it was described, with its effort levels, so the apps learn them from `/v1/models`. What each service says
  (patai_chat `docs/research/model-providers.md`, checked 2026-11-18):
  - **OpenRouter:** its `/models/user` for the owner's key, which leaves out providers their privacy settings at
    OpenRouter refuse. Batch-only entries, routers and those past their end are left out.
  - **OpenAI:** its list only names models, so [models.dev](https://models.dev), an open table keyed by OpenAI's names,
    says what each takes, calls and costs. Asked in OpenAI's Responses, the newest call tools at any effort.
  - **Anthropic:** its list has each model's capabilities, PDFs and effort levels among them; models.dev its price.
  - **Intelligence** is the Artificial Analysis Intelligence Index from OpenRouter's public list, matched to OpenAI's and
    Anthropic's names through models.dev, and credited in the Intelligence header's tooltip (Patrick, 2026-11-18: for
    the prototype and personal use). A model without one shows none. The unit reads
    both public lists when the table opens, without a key, and keeps them an hour; patai ships no copy. Before patai
    ships to many owners, Artificial Analysis should be asked whether showing it is allowed (its terms forbid embedding
    its data in a product without a contract).
- **APIs** (Patrick, 2026-11-18: as close to native as the service allows): each model is asked in its maker's own
  where its service takes it, and `/v1/models` says which as `api`. Claude, from Anthropic or through OpenRouter, in
  Anthropic's Messages, `/v1/messages`, where it reads PDFs and takes `output_config.effort`; OpenAI's models, from
  OpenAI or through OpenRouter, in OpenAI's Responses, `/v1/responses`; every other in chat completions. The gateway
  takes all three paths from an app and sends each on as it came, with the service's key the way it takes it (Anthropic
  as `x-api-key`, the others as a bearer token), and refuses an API the service doesn't take: OpenRouter takes all
  three, Anthropic and OpenAI their own and chat completions, any other chat completions. Usage is read from each
  API's own shape.
- **Effort:** in Messages and Responses the app puts it where the API takes it. In chat completions it asks with
  OpenAI's `reasoning_effort`, and the gateway sends it on only if the model lists that level: to OpenRouter as
  `reasoning: {effort}`, which it passes on as each maker's own; to any other service as it came. A level the model
  doesn't list is dropped, and the service's default answers.
- **Nothing is kept at the service:** Responses is always sent `store: false`, which OpenRouter requires; OpenAI's chat
  completions get it unless the app said otherwise, as OpenAI keeps them by default on accounts made since 2025.

- **Names:** an app asks for one as `<service>/<the service's name>`, such as `openrouter/anthropic/claude-opus-5.5`.
  `/v1/models` lists those it may use after the unit's own, each with `"outside": true`, the service's name as
  `owned_by`, the model's own `name`, its `architecture.input_modalities` (`file` for one that reads PDFs), its
  `efforts`, lowest first, its `default_effort`, and its `api`: `messages`, `responses` or `chat`.
- **The gateway** reads a request's model, in any of the three APIs, and sends one for an outside model to its service with the key, under
  the service's own name for it, with no cookie, forwarded address or app key. Any other goes to the unit's own model.
  Each is logged as `outside`, with its tokens, and never with what was asked or answered. A key's limits can leave
  `outside` out.
- **What's sent** is the app's to decide. Chat sends a service nothing personal the person didn't put in the chat
  themselves: not what patai remembers, their settings, name, project or computer. A project's file goes only when the
  person picks it to send (patai_chat `web-v1.md`, Models from services).
- **The unit's own model** is what a chat uses until a person picks another, and what answers if the owner takes a model
  away.

## The terminal

`workloads/open-terminal` is the Chat app's computers, behind its Computer API: Open Terminal 0.17.13's slim image,
built from its release's source, with numpy, pandas, matplotlib, Pillow, openpyxl, python-pptx and python-docx, and
pip. A shell, files and Python, with no sudo, no browser and no desktop. An app with a `person` API at `/terminal/`
gets a computer for each thing it names, such as a chat or a project (patai_chat `docs/research/chat-computers.md`):

- **Names.** The app says whose a request is in `X-User-Id` and which of their computers in `X-Computer`, its own
  name for it, such as `chat:<id>`, and `X-Computer-Kind: temporary` for one that goes when it stops. Only the app
  holds the key that reaches the gateway for apps, so the gateway takes its word. It names the person and the computer
  by parts of hashes, so neither says anything about them.
- **Each computer** is the Compose project `open-terminal_<person>_<computer>`, which `patai-rootd` starts the first
  time it's used and stops after 10 minutes unused, and a folder, `/data/open-terminal/<person>/<computer>`, mounted as
  its home. Beside the folder, `<computer>.json`, which only root reads, has whether it's temporary, the app's name
  for it, and when it was made and last used. A person's folder holds only these, and only root lists it.
- **Kept** until the app deletes it, or 30 days unused: the console has the unit sweep once an hour. A temporary
  computer's folder goes when it stops, and at every boot, before Docker starts.
- **At most three** of a person's run at once: a fourth stops the one used longest ago that isn't busy. When the unit
  has no room for another, as beside Gemma 4 26B, which leaves room for one, the gateway answers 503 `no_room` and
  stops nothing (Patrick, 2026-11-18): the app may offer the person to stop one of theirs that isn't busy. Each still
  counts its 768 MiB in admission; idle, one takes about 64 MiB.
- **`/computers/`** on the gateway for apps, with the app's key and `X-User-Id`: `GET` lists the person's computers by
  the app's names, with their size, when each was last used, whether it runs and whether it's busy (a request to it
  going on, or its terminal open); `POST /computers/<name>/stop` stops one, 409 if it's busy, keeping its files unless
  it's temporary; `DELETE /computers/<name>` deletes one with its files, and `DELETE /computers/` every one. Chat
  calls it when a chat, a project or a person is deleted.
- The gateway sends each request on with that container's own key, made with it, in place of the app's, which never
  reaches a container a person can run commands in. A WebSocket signs in with its first message, so the gateway
  answers the handshake, checks that message and opens its own to the container.
- Each is held to 768 MiB, 2 cores and 512 processes, runs with no new privileges, and is never restarted by Docker.
  Each that runs counts its 768 MiB in admission. There's no shell history, and Open Terminal's record of each command
  is in `/tmp`, so it goes with the container.
- Its network, `patai-terminals`, lets no container reach another, and the firewall gives it the internet and DNS but
  not the LAN or the node itself.
- Every container's host is called `terminal`.
- Measured on patai-dev, under Open WebUI: an image of 84 MB to pull, a container answering in about 7 s and taking
  48 MiB idle, before the Python libraries went in.

## Memory

What the apps that are on need has to fit the **26 GiB kept for apps**. A change that would take it past that, and
further than it was, is refused, naming what takes what. Each service's peak is measured alone: a pinned language
model's at a size plus a rate for each 1k of context, and what each request at once keeps of its checkpoints, the image
engine's for each largest size, anything else's by its profile. An owner's own model is estimated from its file until it's measured. Peaks are measured with nvmap's pool
counted, so added up they run high, and 26 GiB leaves at least 3.4 of the 30.3 the OS doesn't use.

## Measured

- **Container memory limits don't cover GPU allocations.** CUDA memory comes from NVIDIA's nvmap, outside the
  cgroup, so a limit catches a container's CPU memory only. Admission is what keeps the GPU's use in bounds.
- **The console stays responsive under pressure:** with 23 GiB of GPU memory held and every core busy, it answered
  every call, in 15 ms at the median.
