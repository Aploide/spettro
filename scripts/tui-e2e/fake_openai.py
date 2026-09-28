#!/usr/bin/env python3
"""Fake OpenAI-compatible server for driving the Spettro TUI end to end.

It answers GET /v1/models with one model and POST /v1/chat/completions with a
scripted sequence of streamed responses, so no real provider (and no API
credit) is used. The step is chosen by how many tool results the request
already carries: the first request gets SCRIPT[0], the request that carries
its result gets SCRIPT[1], and so on. Requests without tools (session titles
and other side calls) get a short text answer.

The script is the worst case for the TUI: huge tool arguments (a 2000-line
heredoc, a whole generated file, a 3000-character edit), hostile output
(a 10k-character line, tabs, colour escapes, a carriage-return progress
meter, 400 lines), a long todo list, an ask-user form with long texts, and a
final answer with an over-wide code block and table.

Every request is appended to LOGFILE as one JSON line.

Usage: fake_openai.py PORTFILE LOGFILE
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

FILE_LINES = "".join(f"\tfunc f{i}() string {{ return \"{'x' * 30}\" }}\n" for i in range(2000))
HEREDOC = "cat <<'EOF' > big.go\npackage big\n" + FILE_LINES + "EOF\nwc -l big.go"
HOSTILE = (
    "python3 -c \"print('A'*10000)\"; "
    "printf 'col1\\tcol2\\t\\033[31mred\\033[0m\\n'; "
    "printf 'progress 10%%\\rprogress 50%%\\rprogress 100%%\\n'; "
    "seq 1 400"
)
DEEP_PATH = "internal/" + "deep/" * 25 + "generated_file.go"
TODOS = [
    {"id": f"t{i}", "content": f"task {i}: " + "do the thing with a very long description " * 4,
     "status": "in_progress" if i == 0 else "pending"}
    for i in range(10)
]
QUESTION = {
    "header": "Scope",
    "question": "Which of these very long options should the agent take next? "
                + "Consider every trade-off carefully. " * 12,
    "options": [{"label": f"Option {i}: " + "a long label " * 6, "description": "a long description " * 12}
                for i in range(8)],
}
FINAL_ANSWER = (
    "All done.\n\n```go\n\tfunc long() { return \"" + "L" * 300 + "\" }\n```\n\n"
    "| a | b |\n|---|---|\n| " + "cell " * 40 + " | x |\n"
)

# (kind, tool name, arguments or text), one entry per model turn.
SCRIPT = [
    ("tool", "bash", {"command": HEREDOC, "description": "write a huge file with a heredoc"}),
    ("tool", "file-write", {"path": DEEP_PATH, "content": "package deep\n" + FILE_LINES}),
    ("tool", "bash", {"command": HOSTILE}),
    ("tool", "file-edit", {"path": "big.go", "edits": [
        {"old_string": "func f1() string", "new_string": "func f1Renamed() string /* " + "N" * 3000 + " */"}]}),
    ("tool", "todo-write", {"todos": TODOS}),
    ("tool", "ask-user", {"questions": [QUESTION]}),
    ("text", None, FINAL_ANSWER),
]


def log(path, obj):
    with open(path, "a") as f:
        f.write(json.dumps(obj) + "\n")


class Handler(BaseHTTPRequestHandler):
    log_path = None

    def log_message(self, *args):
        pass

    def _json(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        log(self.log_path, {"method": "GET", "path": self.path})
        if self.path.startswith("/v1/models"):
            return self._json(200, {"object": "list", "data": [
                {"id": "fake-model", "object": "model", "owned_by": "fake"}]})
        self._json(404, {"error": "not found"})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        messages = body.get("messages") or []
        tool_results = sum(1 for m in messages if m.get("role") == "tool")
        log(self.log_path, {"method": "POST", "path": self.path, "stream": body.get("stream"),
                            "tool_results": tool_results, "n_messages": len(messages),
                            "tools": [t.get("function", {}).get("name") for t in body.get("tools") or []]})
        if not self.path.startswith("/v1/chat/completions"):
            return self._json(404, {"error": "not found"})
        if not body.get("tools"):
            return self.answer_text(body, "Fake title")
        kind, name, payload = SCRIPT[min(tool_results, len(SCRIPT) - 1)]
        if kind == "tool":
            return self.answer_tool(body, name, payload, tool_results)
        return self.answer_text(body, payload)

    # --- streaming helpers (OpenAI chat.completion.chunk format) ---

    def chunk(self, delta, finish=None):
        payload = {"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": int(time.time()),
                   "model": "fake-model", "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
        self.wfile.write(b"data: " + json.dumps(payload).encode() + b"\n\n")
        self.wfile.flush()

    def start_stream(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

    def end_stream(self):
        usage = {"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": int(time.time()),
                 "model": "fake-model", "choices": [],
                 "usage": {"prompt_tokens": 1000, "completion_tokens": 100, "total_tokens": 1100}}
        self.wfile.write(b"data: " + json.dumps(usage).encode() + b"\n\n")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def answer_tool(self, body, name, args, n):
        call = {"id": f"call_{n}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}
        if not body.get("stream"):
            return self._json(200, {"id": "chatcmpl-fake", "object": "chat.completion", "model": "fake-model",
                                    "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
                                        "role": "assistant", "content": None, "tool_calls": [call]}}],
                                    "usage": {"prompt_tokens": 1000, "completion_tokens": 100, "total_tokens": 1100}})
        self.start_stream()
        self.chunk({"role": "assistant", "content": ""})
        self.chunk({"tool_calls": [{"index": 0, "id": call["id"], "type": "function",
                                    "function": {"name": name, "arguments": ""}}]})
        raw = call["function"]["arguments"]
        for i in range(0, len(raw), 4096):  # arguments arrive in pieces, as from a real model
            self.chunk({"tool_calls": [{"index": 0, "function": {"arguments": raw[i:i + 4096]}}]})
        self.chunk({}, "tool_calls")
        self.end_stream()

    def answer_text(self, body, text):
        if not body.get("stream"):
            return self._json(200, {"id": "chatcmpl-fake", "object": "chat.completion", "model": "fake-model",
                                    "choices": [{"index": 0, "finish_reason": "stop",
                                                 "message": {"role": "assistant", "content": text}}],
                                    "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
        self.start_stream()
        self.chunk({"role": "assistant", "content": ""})
        for i in range(0, len(text), 64):
            self.chunk({"content": text[i:i + 64]})
        self.chunk({}, "stop")
        self.end_stream()


def main():
    port_file, log_path = sys.argv[1], sys.argv[2]
    Handler.log_path = log_path
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    with open(port_file, "w") as f:
        f.write(str(srv.server_address[1]))
    srv.serve_forever()


if __name__ == "__main__":
    main()
