"""Ask the running llama-server one question and report its own timings.

Runs in the same container as the server, so it needs nothing beyond the standard library.
"""
import json
import sys
import urllib.request

question = " ".join(sys.argv[1:]) or "In three sentences, why does a GPU run neural networks faster than a CPU?"
body = {"messages": [{"role": "user", "content": question}], "max_tokens": 200, "temperature": 0.2}
request = urllib.request.Request(
    "http://127.0.0.1:8080/v1/chat/completions", json.dumps(body).encode(), {"Content-Type": "application/json"}
)
with urllib.request.urlopen(request) as response:
    reply = json.load(response)

print(reply["choices"][0]["message"]["content"].strip())
timings = reply["timings"]
print(
    f"prompt {timings['prompt_n']} tokens at {timings['prompt_per_second']:.1f} t/s, "
    f"reply {timings['predicted_n']} tokens at {timings['predicted_per_second']:.1f} t/s"
)
