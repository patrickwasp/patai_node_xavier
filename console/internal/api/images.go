package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"patai/console/internal/store"
)

// images is POST /v1/images/generations, in OpenAI's shape, made by stable-diffusion.cpp's sd-server (docs/apps.md,
// Images). The GPU is shared, not divided: an image takes it for seconds to a minute, so they go one at
// a time, with a short queue that answers 429 when it's full. Each image is a job of sd-server's, and a request whose
// client has gone has its job cancelled, queued or running. Images come back as b64_json only: the unit keeps none.
// The engine's settings say the largest size, steps, guidance, sampler and how many may wait (docs/apps.md,
// Advanced): its memory was measured to that size.
const (
	imagesMax     = 4                // images in a request
	imagePrompt   = 4000             // characters
	imageDeadline = 10 * time.Minute // for one image
	imagePoll     = 500 * time.Millisecond
)

// The sizes a request can ask for, up to the largest the settings allow. FLUX.2 klein takes multiples of 16.
var imageSizes = map[string][2]int{"512x512": {512, 512}, "768x768": {768, 768}, "1024x1024": {1024, 1024},
	"512x768": {512, 768}, "768x512": {768, 512}, "768x1024": {768, 1024}, "1024x768": {1024, 768}}

type imaging struct {
	sync.Mutex
	waiting int
	slot    chan struct{}
}

type imageRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	// Not OpenAI's, and left out by its clients.
	NegativePrompt string  `json:"negative_prompt"`
	Seed           *uint32 `json:"seed"`
}

// nearestSize is the size of those allowed that's nearest the one asked for: of the same shape as far as can be, then as
// near its size. A model picks its own size, and doesn't know which the unit can make. Auto is 512×512.
func nearestSize(asked string, allowed []string) (string, bool) {
	if asked == "auto" {
		asked = "512x512"
	}
	if slices.Contains(allowed, asked) {
		return asked, true
	}
	w, h, found := strings.Cut(asked, "x")
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if !found || errW != nil || errH != nil || width < 64 || height < 64 || width > 4096 || height > 4096 {
		return "", false
	}
	best, bestShape, bestArea := "", math.Inf(1), math.Inf(1)
	for _, name := range allowed {
		size := imageSizes[name]
		shape := math.Abs(math.Log(float64(size[0]*height) / float64(size[1]*width)))
		area := math.Abs(math.Log(float64(size[0]*size[1]) / float64(width*height)))
		if shape < bestShape-1e-9 || shape < bestShape+1e-9 && area < bestArea {
			best, bestShape, bestArea = name, shape, area
		}
	}
	return best, best != ""
}

// sizesUpTo are the sizes whose longer side is at most side, by name.
func sizesUpTo(side int) []string {
	names := []string{}
	for _, name := range sortedKeys(imageSizes) {
		if size := imageSizes[name]; max(size[0], size[1]) <= side {
			names = append(names, name)
		}
	}
	return names
}

func (s *Server) images(w http.ResponseWriter, r *http.Request, rt route, v *view, rec *store.APIRequest) {
	if r.Method != http.MethodPost {
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "send a POST")
		return
	}
	var req imageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := dec.Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "that request couldn't be read")
		return
	}
	if req.N == 0 {
		req.N = 1
	}
	if req.Size == "" {
		req.Size = "512x512"
	}
	sizes := sizesUpTo(int(v.number(rt.service, "size", 512)))
	name, sized := nearestSize(req.Size, sizes)
	size := imageSizes[name]
	switch {
	case strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > imagePrompt || len(req.NegativePrompt) > imagePrompt:
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_prompt", fmt.Sprintf("give a prompt of up to %d characters", imagePrompt))
		return
	case req.N < 1 || req.N > imagesMax:
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_n", fmt.Sprintf("n can be 1 to %d", imagesMax))
		return
	case !sized:
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_size", "size is width x height, such as "+strings.Join(sizes, ", "))
		return
	case req.ResponseFormat != "" && req.ResponseFormat != "b64_json":
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_response_format", "only b64_json: the unit keeps no images")
		return
	}
	// The service runs one image model, its first. A request's model, such as dall-e-2, is taken as that one, as a chat
	// model's name is.
	models := v.models[rt.service]
	if len(models) == 0 {
		apiError(w, http.StatusServiceUnavailable, "server_error", "no_model", "the images API has no model yet")
		return
	}
	rec.Model = models[0]

	release, ok := s.imageTurn(w, r.Context(), int(v.number(rt.service, "waiting", 2)))
	if !ok {
		return
	}
	defer release()
	engine := &sdClient{base: rt.target, client: &http.Client{Timeout: time.Minute}}
	data := []map[string]string{}
	for i := 0; i < req.N; i++ {
		seed := rand.Uint32()
		if req.Seed != nil {
			seed = *req.Seed + uint32(i)
		}
		png, err := engine.make(r.Context(), map[string]any{"prompt": req.Prompt, "negative_prompt": req.NegativePrompt,
			"width": size[0], "height": size[1], "seed": seed, "batch_count": 1, "output_format": "png",
			"sample_params": map[string]any{"sample_steps": int(v.number(rt.service, "steps", 4)),
				"sample_method": v.choice(rt.service, "sampler", "euler"),
				"guidance":      map[string]any{"txt_cfg": v.number(rt.service, "guidance", 1)}}})
		if err != nil {
			if r.Context().Err() != nil {
				// Nobody to answer. 499 is what the log says for a client that went.
				rec.Status, rec.Images = 499, int64(len(data))
				return
			}
			apiError(w, http.StatusBadGateway, "server_error", "image_failed", err.Error())
			rec.Images = int64(len(data))
			return
		}
		data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(png)})
	}
	rec.Images = int64(len(data))
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "size": name, "data": data})
}

// imageTurn waits for the GPU's image slot, behind a short queue.
func (s *Server) imageTurn(w http.ResponseWriter, ctx context.Context, waiting int) (func(), bool) {
	s.imaging.Lock()
	if s.imaging.slot == nil {
		s.imaging.slot = make(chan struct{}, 1)
	}
	slot := s.imaging.slot
	select {
	case slot <- struct{}{}:
		s.imaging.Unlock()
		return func() { <-slot }, true
	default:
	}
	if s.imaging.waiting >= waiting {
		s.imaging.Unlock()
		w.Header().Set("Retry-After", "30")
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "images_busy", "the unit is making other images: try again soon")
		return nil, false
	}
	s.imaging.waiting++
	s.imaging.Unlock()
	defer func() {
		s.imaging.Lock()
		s.imaging.waiting--
		s.imaging.Unlock()
	}()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-ctx.Done():
		return nil, false
	}
}

// sdClient speaks sd-server's own API, whose images are jobs: made, looked at until they're done, and cancelled.
type sdClient struct {
	base   *url.URL
	client *http.Client
}

// sdJob is a job as sd-server says it stands.
type sdJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result *struct {
		Images []struct {
			B64 string `json:"b64_json"`
		} `json:"images"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *sdClient) call(ctx context.Context, method, path string, body any, into *sdJob) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	u := *c.base
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("the image engine isn't answering")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		var refused struct {
			Error any `json:"error"`
		}
		json.Unmarshal(data, &refused)
		if message, ok := refused.Error.(string); ok && message != "" {
			return fmt.Errorf("the image engine refused it: %s", message)
		}
		return fmt.Errorf("the image engine answered %d", resp.StatusCode)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(data, into)
}

// make has sd-server make one image and waits for it. If ctx ends first, the job is cancelled, queued or running.
func (c *sdClient) make(ctx context.Context, params map[string]any) ([]byte, error) {
	var job sdJob
	if err := c.call(ctx, http.MethodPost, "/sdcpp/v1/img_gen", params, &job); err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, fmt.Errorf("the image engine didn't take it")
	}
	path := "/sdcpp/v1/jobs/" + url.PathEscape(job.ID)
	deadline := time.Now().Add(imageDeadline)
	for {
		select {
		case <-ctx.Done():
			c.cancel(path)
			return nil, ctx.Err()
		case <-time.After(imagePoll):
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &job); err != nil {
			if ctx.Err() != nil {
				continue
			}
			return nil, err
		}
		switch job.Status {
		case "completed":
			if job.Result == nil || len(job.Result.Images) == 0 {
				return nil, fmt.Errorf("the image engine made no image")
			}
			return base64.StdEncoding.DecodeString(job.Result.Images[0].B64)
		case "failed", "cancelled":
			if job.Error != nil && job.Error.Message != "" {
				return nil, fmt.Errorf("the image engine couldn't make it: %s", job.Error.Message)
			}
			return nil, fmt.Errorf("the image engine couldn't make it")
		}
		if time.Now().After(deadline) {
			c.cancel(path)
			return nil, fmt.Errorf("the image took too long")
		}
	}
}

// cancel stops a job, queued or running. It has its own few seconds: the request's are over.
func (c *sdClient) cancel(path string) {
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	c.call(ctx, http.MethodPost, path+"/cancel", map[string]any{}, nil)
}
