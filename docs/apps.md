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
| `models` | Pinned models: `name`, `title`, whether `default` (kept unless the owner says otherwise), `dir`, `sha256`, `size`, `url`, the `mirror` that has a whole copy, and the `notices`, its licence and any notices in the recipe's `notices/`, that go beside the copy. `files` are more a model needs beside it, such as a vision projector or an MTP draft, kept and removed with it. `env` is what it needs over the profile, `settings` its defaults, `limits` its bounds, and `memory` its measured peak: `base_mib`, `kv_mib_per_1k` and `request_mib`, what each request at once keeps of its checkpoints |
| `model` | How it takes models: `env` names the setting a one-at-a-time service reads its model from, `dir` where owners' own go, and `formats` what they can be (`gguf`, `safetensors`) |
| `profiles`, `profile` | Settings sets with what each takes in memory, and the default one |
| `memory` | For a service whose peak goes by a setting rather than a model, such as the image engine by largest size |
| `settings` | What an owner can change: a name, title, type (`int`, `number`, `choice`, `bool`), range or choices, default, and the variable it sets, or `for: gateway` |
| `secrets` | Variables the owner gives, such as a search provider's key, `made` ones the service makes for itself, or ones given `once`, such as the password of an account it makes at its first start |
| `data` | A folder of its own, `/data/<service>`, made for `uid`, which every backup holds whole (`console.md`, Backups). `backup: link` says the service writes each file once and never changes it, so a backup takes links to them while it's paused, at once, rather than copies. `backup: postgres`, with `database` and `user` as its compose file makes them, says it's a Postgres server's folder, which a backup holds as a dump that any node's Postgres loads; its image loads one left in its empty folder at its first start |

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

## The internet

A service reaches outside the node only if it needs to. `patai-firewall` refuses every packet from a bridge Docker
named, `br-…`, to anywhere but a bridge, so a service on Compose's own network or an internal one reaches the
containers beside it and the node, and nothing else: no LAN, no internet, and no name looked up outside the node.
Models, images and updates are the node's own to download, never a container's.

| Network | Way out | Services on it |
| --- | --- | --- |
| `patai-web` | The LAN and the internet | SearXNG, to its search engines; the Chat server, to the pages it reads, Gmail, connectors and outside models; the agentic API, to a search provider and the MCP servers a request names |
| `patai-terminals` | The internet and DNS, not the LAN or the node | Each person's computer (The terminal) |
| `patai-ai`, `patai-chat` | None: internal | The model, SearXNG and the agentic API; the Chat server and its Postgres |
| A service's own | None | llama.cpp, embeddings, images, speech to text, Piper, Chat's pages |

- `patai-web` and a person's network are bridges named for their network, where no container reaches another, and
  each other network a service shares is internal, so a service on `patai-web` goes out through it and no other. Such a
  recipe isn't on its own network as well, which Docker could pick as its way out. `patai-workload` makes a network
  only when it isn't there, so one an earlier release made stays as it was until it's removed.
- `console/internal/workloads/manifest_test.go` holds the list of services on `patai-web`, and each network a recipe
  joins to its manifest, so `patai-workload` makes it.
- `docker0` stays open, for a container run by hand, such as a build's: Compose never puts a service there.
- **Names.** Docker's resolver, at 127.0.0.11 in each container, answers the names of the containers beside it, and
  asks the node's own resolver for any other, from the node, where the firewall can't tell it from the node's own
  lookups. So each recipe that isn't on `patai-web` and isn't a person's names `dns: [192.0.2.1]`, an address kept for
  documentation that nobody has: Docker then asks it from the container, the firewall refuses it, and the name fails
  at once. The containers' names still answer. An internal network asks nobody, so Chat's Postgres has it only to be
  like the rest. `manifest_test.go` holds each recipe to it.
- So llama.cpp doesn't fetch an image a request names by its web address, as llama-server would: send it as data, as
  Chat does.
- **SearXNG asks the internet only what a search asks**, from its image 2026.11.12-2: its settings leave out the radio
  browser engine, which looks up its servers each time SearXNG starts, and the tracker URL remover, which fetches
  ClearURLs' rules from jsDelivr then. They name patai's page, the repository's, as the contact that Wikipedia, Wikidata
  and OpenStreetMap ask to find in a User-Agent, which SearXNG then sends them, so those engines start.
- **No usage data is sent by anything in a recipe** (checked 2026-11-20, against the pinned sources): Cactus's engine
  (Voice), Piper's ONNX Runtime, which is told not to and pinned with Piper's other packages, llama.cpp,
  stable-diffusion.cpp, the agentic API, whose OpenTelemetry export is off unless set, and Chat, which turns off
  Next.js's and better-auth's. pip in a person's computer doesn't ask PyPI for a newer pip.

### Measured

On patai-dev, 2026-11-20, release `2026.11.20-egress1`, with every packet that left a bridge logged and conntrack read
each second:

| Service | Out of its bridge |
| --- | --- |
| llama.cpp, embeddings, speech to text, Piper, Chat's pages | Nothing: a connection to the internet, the LAN or a name server is refused at once, and the gateway for apps on 18000 answers. The node still looked up a name for them, as this release had no `dns: [192.0.2.1]` |
| Chat's Postgres | Nothing, on an internal network with no route out |
| SearXNG | Its search engines during a search, and jsDelivr when it starts (above). With the new settings, nothing from its start until a search |
| The Chat server | The page it was asked to read |
| The agentic API | Nothing while Chat was used |

Chat's web search, page reading, text to speech and speech to text each worked, again with Piper on
`dns: [192.0.2.1]`, which then failed `example.com` in under 10 ms and still found `piper-server-1`. Before, every
container reached the internet, the LAN and any name.

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
- **Models** are mirrored whole in Wasupu's bucket `wasupu-xavier-rogue` on OVHcloud (BHS, One Zone, Standard), at
  `models/<sha256>/<name>`, which is a model's `mirror`: `https://wasupu-xavier-rogue.s3.bhs.io.cloud.ovh.net/models/<sha256>/<name>`.
  Beside each file are the `notices` its model names, as `LICENSE`, `USAGE_POLICY`, `MODEL_CARD` or `NOTICE`, from
  the recipe's `notices/` (`notices/SOURCES` says where each came from). The Mirror models workflow
  (`scripts/mirror-models`) streams each file from its `url` on GitHub's side, as 64 MiB parts of one multipart
  upload, hashes it as it goes, and aborts the upload if it doesn't match its pin. Run again, it carries on from the
  parts it uploaded, reading them from the `url` again only to hash them. Run it after pinning a model with a
  `mirror`. Objects are public-read and the bucket can't be listed. OVH bills its storage, about CA$0.01 a GiB a
  month, and nothing for downloads or requests, so nothing is in front of it and only `patai-fetch` limits a node, to
  six ranges at a time. The workflow has copies of the bucket's S3 keys as the Actions secrets
  `WASUPU_OVH_XAVIER_ROGUE_S3_ACCESS_KEY` and `WASUPU_OVH_XAVIER_ROGUE_S3_SECRET_KEY`; the keys themselves are in
  Infisical. Liquid AI's LFM models have no `mirror` while redistributing them is undecided, and come from their own
  address. The release `models`, which held models in 1 GiB pieces before, is no longer read. Proven on patai-dev as
  2026.11.20-models1: Whisper large-v3 turbo (547 MiB, 9 ranges) came from the bucket, was removed with
  `patai-workload model remove`, was stopped with 3 ranges done, and resumed with 369 MiB received of 547, matching
  its pin.

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
- **`GET /node` on the gateway for apps** answers an app's key, and nothing else, with what a phone needs to find and
  trust the unit, for Chat's Connect phone: `id`, a random UUID the console makes once in `/persist/console/node-id`
  (in backups, new after a factory reset), `name`, the unit's own certificate's `fingerprint`, and `addresses`:
  `<name>.local`, each port's IPv4 address, then the tailnet name while Tailscale is on.
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
  against their release's manifest. It makes no connection: of the network it has only a server's, which isn't
  started here, and it reads no environment and names no address. The anonymous usage counts Cactus's README speaks of
  are its Python package's, which isn't in the image; `CACTUS_TELEMETRY=0` is set anyway. Its source for 4.1.0, commit
  89a1c50, isn't public, so that's read from the binary (checked 2026-11-20). Whisper small at 8 bits and large-v3 turbo at 5 bits are choices, in 99 languages
  and better with names and numbers. With CUDA each takes more than its file, 1.26 and 1.58 GiB: beside the 26B and Chat,
  small fits with 62 MiB to spare, and turbo doesn't. On the CPU alone small took 15 to 25 s for an 11 s recording, so it
  stays on the GPU.
- **Text to speech** is Piper on 4 of the CPU's cores, behind `patai-speech`, the recipe's own, as Piper's server can
  download voices: `input` of up to 4,096 characters, `voice` and `speed`. It answers in WAV only, as the image has no
  encoder; a request for another format is refused. A voice it doesn't have, such as OpenAI's `alloy`, gets the
  default. It runs as root in its container, with every capability taken away: `patai-fetch` keeps models as root's
  alone. Its image sets `ORT_DISABLE_TELEMETRY=1`: ONNX Runtime 1.32 otherwise sends Microsoft telemetry from Linux
  too, which it did on patai-dev.
- **Its voices** are Piper's medium ones whose recordings are public domain or CC0, and Cori's, CC BY 4.0: Cori is
  the voice of Cori Samuel, a LibriVox reader, trained on her recordings in NVIDIA Corporation's Hi-Fi Multi-Speaker
  English TTS Dataset, which is under CC BY 4.0 (`workloads/piper/notices/cori/NOTICE`, beside the voice in the
  bucket). LJ Speech and
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
  loopback reaches only agentic-api's own. It goes out through `patai-web`, to a search provider and the MCP servers a
  request names (The internet).
- Usage is read only where an answer puts it, so a request can't set its own count. A WebSocket is refused, since its
  frames would pass uncounted.
- Stored responses and conversations aren't tied to the key that made them: any key that knows an id can use one.
- It runs the Python a model writes in its code interpreter (below).

### The code interpreter

The agentic API runs Python a model writes when a request gives it the tool, as OpenAI's Responses does:
`{"type": "code_interpreter", "container": {"type": "auto"}}`. It's agentic-api's own, in
[Eryx](https://github.com/eryx-org/eryx) 0.15.2: CPython 3.14 compiled to WebAssembly, run by Wasmtime 50.0.3. The
model sees a function, `code_interpreter(code)`, and the answer has a `code_interpreter_call` with the code, its
`status`, what it printed as `logs`, any image it saved as an `image` with the file's URL, and its `container_id`.
Research and what was rejected: `research/code-interpreter.md`; the packages: `research/interpreter-packages.md`.

- **What it gives:** stdout and stderr, and the files in `/mnt/data`, which the model is told to save what it makes
  to. Python's standard library, `sqlite3` among it, with openpyxl, xlsxwriter, pypdf, mammoth, tabulate,
  python-dateutil, pytz, tzdata, beautifulsoup4, markdown, sympy, networkx and pygal, pure Python
  (`workloads/agentic-api/wheels.txt`), and NumPy 2.5.5 and pandas 3.1.1, compiled to WebAssembly when the image is
  built (Running it). No matplotlib or Pillow: charts are pygal's, and the tool's description says so. SQLite keeps
  its temporary files in memory: WASI's Python finds no folder it may write them to, and a sort past SQLite's cache
  failed (`_patai_sqlite.py`).
- **A container per conversation**, as OpenAI's are: the folder at `/mnt/data` and the Python variables a call leaves
  carry to the next. `{"type": "auto"}` takes the conversation's last container while it lasts, by
  `previous_response_id`, a conversation or the items a client sends back, and makes one otherwise; `file_ids` put
  Files API files in it. `"container": "cntr_…"` names one. Every call in a response uses the same one, one after
  another. Each call still runs in a fresh worker; the container is on the disk between them.
- **Variables carry** as Eryx pickles them, with dill, up to 10 MiB: a variable that can't be pickled, such as an
  open database connection or a generator, is named to the model as not kept, and so is everything when there's more.
  So is one whose pickle won't load again, such as an openpyxl worksheet after a call: Eryx checks only that each
  pickles, and one that then didn't load lost them all. A call that raises keeps its variables; one a limit stopped
  keeps nothing of its own, and the next has the variables from before it. Files in `/mnt/data` are kept either way.
- **The sandbox is WebAssembly.** The guest has no system calls, only what Eryx gives it: the standard library and
  the packages, read-only, as `/python-stdlib` and `/site-packages`; its container's files at `/mnt/data`; a `/data`
  and `/tmp` of 64 MiB in the worker's memory, gone after the call; and two output streams. No network, not even
  loopback, no processes, no environment but Python's paths, no host calls.
- **Each call is a worker process of its own**, started with nothing in its environment and its output on
  `/dev/null`, talking to the server over a private socket that only it may connect to. It's killed at its deadline,
  when the request is cancelled or its client goes, and the next call gets a new one.
- **Its own cgroup** holds each worker to its limits, below the container's. The server makes them as the container's
  user, which can't change the container's own limits (Running it).
- **Owners turn it off** with the agentic API's Code interpreter setting, on by default. A request that gives the
  tool while it's off is refused before the model runs, and the Containers API answers 404.

| Each run | Limit | Measured on patai-dev |
| --- | --- | --- |
| Memory | 256 MiB for the guest, Wasmtime's; 384 MiB for the worker, its cgroup's, with no swap | The packages take about 110 MiB, leaving 144 for data; a hog's worker peaked at 232 MiB |
| CPU | One core, `cpu.max` | 1.1 s of it to start a call, the packages already imported |
| Processes | 64, `pids.max`; a guest can't make one | 4 each |
| Time | 45 s, and 1.5 × 10¹¹ of Wasmtime's fuel; the gateway gives a tool call 60 s in all | A loop stopped by its fuel at 33 s of a core, a sleep at 44 s |
| Output | 64 KiB of stdout and 64 KiB of stderr kept; more stops the run | |
| Code | 64 KiB | |
| Files | 512 MiB and 1,000 files in its container; a write past them fails | A 23 MB CSV in 1.1 s, and into SQLite in 6.5 s |
| At once | 1, so a computer fits beside the 26B (Patrick, 2026-11-20); another call at the same time fails at once | |

#### Containers

OpenAI's Containers API, on the agentic API's routes, for a key with the agentic API:

| Route | What it does |
| --- | --- |
| `POST /v1/containers` | Make one: `name`, `file_ids`, `expires_after: {"anchor": "last_active_at", "minutes": 1 to 1440}`, 20 by default |
| `GET /v1/containers`, `GET`, `DELETE /v1/containers/{id}` | List, read and delete them |
| `POST /v1/containers/{id}/files` | A file in: multipart with a `file`, streamed to the disk, or JSON with a Files API `file_id` |
| `GET /v1/containers/{id}/files`, `GET`, `DELETE …/files/{file_id}` | Its files, with the ones a call made as `source: "assistant"` |
| `GET …/files/{file_id}/content` | A file's bytes, typed by its name, as an attachment |

- **Big files go straight into a container.** The Files API takes 20 MiB a file; a container takes 512 MiB.
- **A container lasts** until it's been idle for its expiry: a call, or a file put in or taken out, is activity. The
  unit keeps 16 at most, on the disk in `/data/agentic-api/containers`, and an expired one is removed when the next
  request comes.
- **Like stored responses, they aren't tied to the key that made them:** any key that reaches the agentic API can list
  and read every container and file. A unit's keys are its owner's.
- **The gateway counts no tokens for them,** nor for the Files API: they're stored, not read by a model, and an upload
  would otherwise count by its size.

#### Running it

Docker mounts a container's cgroup read-only, so the server couldn't make a worker's. The recipe runs on Docker's
runtime `patai-delegate`, `/usr/local/sbin/patai-runc`, which is runc with two things more when it creates the
container:

- the container's `/sys/fs/cgroup` is writable. In its own cgroup namespace that's the container's cgroup and what's
  below it, nothing else.
- `delegate/`, below it, is the container's user's, as systemd delegates one: the folder and its `cgroup.procs`. The
  container's processes are in `delegate/main`, and the server makes each worker's cgroup beside it, with the memory,
  pids and cpu controllers on, up to 64. Everything else stays root's: the limits of `delegate/`, and the container's
  own cgroup, which runc gives the container's user once its mount is writable and `patai-runc` gives back, so
  nothing can be made beside `delegate/`.

The node's cgroup2 is mounted with `nsdelegate`, so the kernel refuses a write from inside a container to the resource
files at the top of its namespace, its own limits. Below that only file ownership protects them, so nothing in the
container may be root: `patai-runc` refuses a container that runs as root or in root's group, has a capability, may gain
privileges, or shares the host's cgroup namespace. The recipe runs as 10001 with every capability dropped,
`no-new-privileges` and a read-only root. Its `/tmp` is a 16 MiB tmpfs. The workers' own temporary files are in
`TMPDIR`, `/data/agentic-api/tmp`, on the disk: the first unpacks Eryx's runtime and the standard library there for
the others to map, as the runtime is 235 MB with NumPy and pandas. The container is held to 1 GiB and 256 processes. A
run at its limit took its own memory, as the console counts it, resident and swapped, to 234 MiB (`memory_mib` 256);
with the runtime's file pages, which the kernel takes back, it was 719. `patai-rootd` reads its memory from its scope,
workers included.

The image is built on a PC for arm64 (`workloads/agentic-api/Dockerfile`), from agentic-api v0.10.0 with
`patai.patch`:

- Eryx 0.15.2 and Wasmtime 50.0.3, for Cranelift's fixes, one of them for aarch64 (GHSA-mghf-wjfp-p5xm, critical), and
  Eryx's sessions, snapshots and storage behind a mount;
- containers: the store, `/mnt/data` on the disk held to its caps, the variables carried, and the Containers API;
- a variable kept only if its pickle loads again;
- a worker killed without `cgroup.kill`, which Linux has from 5.14;
- one core per run;
- `patai-eryx-precompile`, which links the packages' WebAssembly extensions into Eryx's runtime, pre-initializes it
  with the packages imported, so a call doesn't import them again, and compiles it for the Xavier's cores. Without it
  sympy and networkx ran out of fuel being imported.

NumPy and pandas, the newest releases, are built in the Dockerfile's first stage, as nobody publishes them for CPython
3.14 on WASI: each sdist, checked against its hash, cross-compiled for wasm32-wasip2 with meson (`wasm-cross.ini`,
`wasm-cc.sh`) against dicej's CPython 3.14 built for WASI with dynamic linking, each extension a shared library. NumPy
has no BLAS, threads or SIMD, and its linear algebra is its own C. Fixed for WebAssembly (`wasm-packages.py`,
`wasm-cxa.c`):

- `np.save` and `np.load` write and read a file as a stream, as `ndarray.tofile()` needs `dup()`, which WASI hasn't
  got;
- pandas imports `ctypes` and `mmap`, which WASI's Python hasn't got, where they're used: `memory_map=True` says it
  can't, and the clipboard fails to import. `read_csv` reads a big file in one thread, as on Emscripten;
- wasi-sdk's C++ has no exceptions, so NumPy's FFT and `unique` and pandas' rolling windows get a `throw` that aborts:
  the call fails where native code would raise.

NumPy 1.26 and pandas 2.3 built with `setup.py`, as dicej/wasi-wheels does, came first. They needed five of NumPy's
getters fixed, which 2.5 has fixed itself, and took 107 MB to 2.5's and 3.1's 43.

#### Measured

On patai-dev at MAXN, through the gateway with a key for the agentic API only, Gemma 4 26B writing the calls: the
sandbox with releases 2026.11.19-ci1 and -ci2, containers, files and packages with 2026.11.20-ci3 to -ci5, and NumPy
and pandas at one run of 256 MiB with 2026.11.20-ci6 and -ci7. A call answered in 6 to 23 s, the model's time
included, and longer when Gemma repeated a long output.

- **A computer fits beside the 26B again.** With the agentic API counted at 256 MiB, the apps' budget kept 1,086 MiB
  free with the 26B loaded. A member's chat computer started in 9.5 s and ran its code, and with it running Gemma 4 26B
  answered through the gateway in 1.3 s, with 8,976 MiB available on the node. With 640 counted, it said `no_room`.
- **How it was counted:** the container's `anon` in `memory.stat`, which takes in its workers' cgroups, and
  `memory.swap.current`, sampled every 5 ms through a hog at its limit and the analyses of 700,000 rows, as
  `patai-rootd` counts a container's own memory. It peaked at 234 MiB. `memory.current` peaked at 719: the runtime
  unpacked to the disk and the server's binary, which embeds it, are file pages the kernel takes back.

- **NumPy and pandas** are there at once: imported in 0.003 s. A 6.3 MB CSV of 200,000 rows was read by `read_csv` in
  0.44 s, and grouped, pivoted and described in 0.88 s; 700,000 rows in 0.84 s and 1.6 s. A chart pygal made of the
  result came back as an `image`, and the next call had the variables. 144 MiB of the 256 is left for data. On the PC
  a broad test passed too: linear algebra, FFT, statistics, `np.save`, and pandas' resample, rolling, ewm, merge,
  pivot, cut, string methods, time zones, query, JSON, Excel and pickling.
- **Gemma uses pandas now.** Asked for an analysis in its own words, it read the file with pandas, then took five
  calls: a column it named in capitals, `render_to_png`, which needs cairosvg, and matplotlib, before pygal's SVG.

- **It runs real Python.** Fibonacci to 200, the 9,592 primes below 100,000 by trial division, SHA-256, `statistics`,
  `fractions`, `json`, `collections`, `datetime` and `re` all gave the right answers. The packages import at once, from
  the pre-initialized runtime.
- **Files:** the guest sees `/python-stdlib`, `/site-packages`, `/mnt/data` and its own `/tmp` and `/data`, and
  nothing else. `/`, `/persist`, `/etc`, `/proc`, `/sys/fs/cgroup`, the Docker socket and agentic-api's database don't
  exist for it, writing to the library is refused, and its environment is `PYTHONHOME` and `PYTHONPATH`.
- **Network:** llama-cpp, SearXNG, loopback (9000, 18090), the gateway for apps and the console on Docker's bridge,
  1.1.1.1 and example.com all gave "networking not enabled for this sandbox", `urllib` too. `getaddrinfo` hands the
  name back unresolved.
- **Processes:** `os.fork`, `os.system` and `os.kill` don't exist, `subprocess` gives "wasi does not support
  processes", there's no `ctypes`, and a thread can't start. A fork bomb fails at its first fork.
- **Runaways are stopped**, `incomplete`: a loop by its fuel at 28 s of a core, a flood of output at 64 KiB, a sleep at
  the run's time. A memory hog gets a `MemoryError`, `failed`. A third call while two ran was refused at once.
- **A container carries a conversation.** A CSV from the Files API was in `/mnt/data` for the first call; a chart
  pygal saved and a workbook openpyxl saved came back as the container's files, by `GET …/content`. Across three calls
  a list of 2,000 rows, a function and a filtered list came back after the middle call raised; the openpyxl workbook
  and worksheet were dropped and named. A container idle past its expiry answered 404.
- **Big files:** a 22 MB CSV went into a container in 1.1 to 1.4 s. In one call SQLite took its 700,000 rows in 6.5 s,
  and a GROUP BY over them in 1.8 s; a query of one region in 1.05 s, an index in 3.2 s. The database was 42 MB.
- **Variables cost time.** 3.2 MB of pickled variables added 16 s of a core to a call that only read them: restoring
  them, checking each loads, and Eryx pickling them twice. Each is now checked with the C pickler first, and one alone
  over Eryx's 10 MiB ends the check, as Eryx would keep none: a call that kept a DataFrame of 200,000 rows took 6.2 s of
  a core. Data is better kept in `/mnt/data`, as a file or in SQLite.
- **The cgroup is a backstop that works.** With the worker's limit set just over the guest's, for the test, the kernel
  killed that worker's cgroup and nothing else (`memory.oom.group`), the call came back `incomplete` and the server
  stayed up.
- **The node didn't notice.** The console's `/api/v1/status` answered in 25 ms (median) and 44 ms at most during two
  loops, against 21 and 48 idle, with CPU pressure at 0.15 % and all nine containers up.
- **From inside the container**, as an escaped guest would be: 10001 with no capabilities, `no-new-privileges` and
  seccomp. Writes to its own `memory.max` and `pids.max`, to `delegate/`'s limits, and moving itself to the top were
  refused, and so was a cgroup beside `delegate/`; in it, the 65th was. No Docker socket, and a read-only root.
- **Found and fixed on the way:** a variable that pickled but wouldn't load again, an openpyxl worksheet, lost every
  variable at the next restore; SQLite failed any sort past its cache with "disk I/O error"; a run that slept to 60 s
  was dropped by the gateway's own 60 s for a tool call, and came back "Code execution failed.".
- **Gemma** won't run code it takes for a fork bomb or an endless loop unless told it's a test, and has said code ran
  when it didn't.

#### Decisions for Patrick

- **The chart in the tool's description.** It says there's no matplotlib; saying pygal saves SVG, with
  `render_to_file`, would save Gemma its failed PNG and matplotlib calls. A new image.
- **Variables or time.** Each call pickles its variables and the next loads them, about 5 s of a core a MB. Near
  Eryx's 10 MiB cap that would pass the gateway's 60 s for a tool call, and the call's output would be lost with it.
  Next: hold saving to a time of its own, check a snapshot by loading it once rather than each variable, and give the
  interpreter's calls a gateway time that covers its own. Or keep a container's worker between calls, as OpenAI does,
  for its memory.
- **matplotlib and Pillow next**, built the way NumPy and pandas are: matplotlib needs FreeType and its C++ with no
  exceptions, Pillow its image libraries (`research/interpreter-packages.md`).
- **Containers aren't tied to keys**, like stored responses: any key that reaches the agentic API can list and read
  every container and file.
- **A named container that's gone** fails the call with "Code execution failed." and a made-up container id. OpenAI
  refuses the request instead.
- **An escaped guest would have agentic-api's network:** llama-cpp, SearXNG and the internet. A worker in a network
  namespace of its own would close that, but needs user namespaces in the container's seccomp profile, which opens
  kernel surface to the same code (`research/code-interpreter.md`, If a guest escaped). Not done.
- **64 KiB of output goes back to the model**, about 8,700 tokens. 16 KiB would save context.
- **The patch.** The kill without `cgroup.kill`, the core per run and the containers could go to agentic-api
  upstream, and checking that a variable loads again to Eryx, whose snapshot checks only that it pickles.

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
or else to the one used least lately. A request an app marks `X-Patai-Priority: background`, which nobody is waiting
on, such as Chat's scheduled work, patbot's and its side calls, the gateway sends to the last slot (`id_slot`) while
Requests at once is 2 or more, so people's conversations keep the others and their cache. An app's sessions at once are
its requests at once: each needs its own cache for
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
  network `patai-chat`, which is internal, and `chat-web` has only its own: the browser calls the API, not the pages'
  server. Only `chat-server` goes out, through `patai-web` (The internet).
- **Photos** people send are kept in `/data/chat-server`, each with copies the server makes for the model and the
  chat, and go with their chat. Photos are offered while the chat model sees.
- **Its memory**, measured on patai-dev 2026-11-17 with 0.2.0: the server 142 MiB at rest, 143 at its peak taking a
  48 MP JPEG, 188 for a 50 MB PNG, and 313 for a 1,000-page PDF, its heap growing while nothing held it back. So its
  heap is held to 192 MiB, which took the PDF to 269, and it counts 384. A recipe's memory is what the console counts
  to admit apps; no container has a limit. Nor does Chat have a memory reservation: under pressure one made no
  difference to it (`console.md`, Memory). Importing a ChatGPT export of 914 MB (4,973 chats, 2,708 files) took the
  server from 170 MiB to 318 at its peak, with 0.16.0 on 2026.11.19-import1: 54 s to upload from a PC on the LAN and
  8 min 51 s to import. The image keeps glibc to two malloc arenas, without which sharp's threads took it 100 MiB
  further. Docker counts the container at up to 1 GiB meanwhile, nearly all of it the page cache of the files written.
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
  2026-07-28. The server reaches a connector itself, through `patai-web`: https on the internet,
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
- **Nothing personal goes on its own:** Chat sends a service's model only what the person typed or attached in the
  chat: not what it remembers, their name or project, and not where their device is, which only the unit's own model
  gets, nor an answer the unit's model wrote knowing it (patai_chat `docs/web-v1.md`, Location).

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
counted, so added up they run high, and 26 GiB leaves at least 3.4 of the 30.3 the OS doesn't use. Within the OS's
share the console's services, Docker's daemons, SSH and mDNS keep their memory under pressure (`console.md`, Memory). No app has
a reservation, and CUDA memory can't have one.

## Measured

- **Container memory limits don't cover GPU allocations.** CUDA memory comes from NVIDIA's nvmap, outside the
  cgroup, so a limit catches a container's CPU memory only. Admission is what keeps the GPU's use in bounds.
- **The console stays responsive under pressure:** with 23 GiB of GPU memory held and every core busy, it answered
  every call, in 15 ms at the median. With its memory protected it loses none of it under pressure (`console.md`,
  Memory).
