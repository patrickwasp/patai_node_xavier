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
| `ui` | A port whose web UI the console serves to people logged in to it |

`workloads/<service>/manifest.json`:

| Field | What |
| --- | --- |
| `name`, `title`, `version`, `licences` | The service, and the licences of what it runs |
| `gpu`, `ready`, `networks` | Whether it uses the GPU, the loopback address of its ready check, and Docker networks it joins |
| `image` | The image on GHCR: `ref`, manifest `digest`, Docker `id`, its `layers` (digest and size) with their total `size`, and its `unpacked` size |
| `models` | Pinned models: `name`, `title`, whether `default` (kept unless the owner says otherwise), `dir`, `sha256`, `size`, `url`, and the `mirror` that has a copy in pieces. `files` are more a model needs beside it, such as a vision projector or an MTP draft, kept and removed with it. `env` is what it needs over the profile, `settings` its defaults, `limits` its bounds, and `memory` its measured peak |
| `model` | How it takes models: `env` names the setting a one-at-a-time service reads its model from, `dir` where owners' own go, and `formats` what they can be (`gguf`, `safetensors`) |
| `profiles`, `profile` | Settings sets with what each takes in memory, and the default one |
| `memory` | For a service whose peak goes by a setting rather than a model, such as the image engine by largest size |
| `settings` | What an owner can change: a name, title, type (`int`, `number`, `choice`, `bool`), range or choices, default, and the variable it sets, or `for: gateway` |
| `secrets` | Variables the owner gives, such as a search provider's key, or `made` ones the service makes for itself |

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
  its manifest names. `show` says which are set, never what they are.
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
- Plain `nvpmodel` leaves containers on cores 0-3 after going up from the default mode; `patai-power-mode` doesn't.
- An app's image carries Compose's project label, so a container run from it by hand inherits it: `serve` counts only
  what Compose made (`com.docker.compose.oneoff=False`).
- Docker's default seccomp refuses calls focal's libseccomp doesn't know, `close_range` among them, with EPERM rather
  than ENOSYS, so a program that falls back only on ENOSYS fails. Such a container needs a profile answering ENOSYS.

## The AI API

`apps/ai-api`: OpenAI-compatible APIs for other programs, with keys, limits and usage.

| API | Service | Routes | Default |
| --- | --- | --- | --- |
| Chat | `llama-cpp`: llama.cpp v1.0.0 | `/v1/chat/completions`, `/v1/completions`, `/v1/responses` | Always on |
| Embeddings | `llama-embed`: llama.cpp and nomic-embed-text v1.5 | `/v1/embeddings` | On |
| Images | `sd-cpp`: stable-diffusion.cpp and FLUX.2 klein 4B | `/v1/images/generations` | Off |
| Agentic API | `agentic-api` with `searxng` | `/v1/responses`, `/v1/conversations`, `/v1/messages` | On |

**Chat's default model** is Gemma 4 26B-A4B QAT, which sees images through its projector and writes faster with its
MTP draft. The other pinned models are an owner's choice, as are their own GGUF files. Each pin has its own defaults,
such as thinking off for Gemma 4 and Qwen3.6.

### The gateway

The console's gateway is the only way in to the services. It routes by path, to the service behind the API that has
it, while that API and its app are on; otherwise it answers 404 in OpenAI's shape. `/v1/models` lists what the APIs
that are on run, and only those a key may use. A model's name isn't routed on: each service runs one model.

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

`POST /v1/images/generations`, in OpenAI's shape, made by stable-diffusion.cpp's `sd-server`. Size, steps and count are
bounded, and images come back as `b64_json` only: the node keeps none.

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
- Its code interpreter needs cgroup v2, and the node boots cgroup v1: CTI's kernel lacks `CONFIG_CGROUP_BPF`, which
  Docker needs on v2 for the GPU.

### Settings

Each setting has a default, and Reset puts them all back. A change restarts only the service it belongs to.

| Chat | Default | Choices |
| --- | --- | --- |
| Context | 32k | 4k to 128k, up to the model's limit, shared by the requests at once |
| Requests at once | 2 | 1 to 4 |
| Thinking | The model's pin | Auto, on or off; effort for gpt-oss |
| Temperature, top-p, top-k, min-p, longest answer | The model's own | For requests that don't set their own |
| 8-bit cache | Off | Halves the cache's memory |

| Images | Default | Choices |
| --- | --- | --- |
| Largest size | 512×512 | 512, 768 or 1024 on a side |
| Steps, guidance, sampler | The model's | For requests that don't set their own |
| Waiting | 2 | Images queued before the next is refused |

llama-server's cache of past prompts is capped at 2 GiB, not a setting: without the cap a node ran out of memory and
hung.

## Memory

What the apps that are on need has to fit the **26 GiB kept for apps**. A change that would take it past that, and
further than it was, is refused, naming what takes what. Each service's peak is measured alone: a pinned language
model's at a size plus a rate for each 1k of context, the image engine's for each largest size, anything else's by its
profile. An owner's own model is estimated from its file until it's measured. Peaks are measured with nvmap's pool
counted, so added up they run high, and 26 GiB leaves at least 3.4 of the 30.3 the OS doesn't use.

## Measured

- **Container memory limits don't cover GPU allocations.** CUDA memory comes from NVIDIA's nvmap, outside the
  cgroup, so a limit catches a container's CPU memory only. Admission is what keeps the GPU's use in bounds.
- **The console stays responsive under pressure:** with 23 GiB of GPU memory held and every core busy, it answered
  every call, in 15 ms at the median.
