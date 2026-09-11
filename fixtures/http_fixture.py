"""envctl HTTP fixture v1: a small, durable JSON record API for isolated tests."""
import contextlib
import fcntl
import http.server
import json
import os
import pathlib
import re
import sys
import tempfile
import urllib.parse

VERSION = 'envctl-http-fixture/1.0.0'
LIMIT = 64 * 1024 * 1024
ROOT = pathlib.Path(os.environ.get('ENVCTL_FIXTURE_DATA', '/data'))
KEY = re.compile(r'^[A-Za-z0-9_.-]{1,128}$')

def encode(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False) + '\n').encode()

def validate(state):
    if not isinstance(state, dict) or set(state) != {'version', 'records'} or type(state['version']) is not int or state['version'] != 1 or not isinstance(state['records'], dict):
        raise ValueError('invalid fixture snapshot')
    if any(not KEY.fullmatch(key) for key in state['records']) or len(encode(state)) > LIMIT:
        raise ValueError('fixture snapshot exceeds its bounds')
    return state

@contextlib.contextmanager
def locked(read=True):
    ROOT.mkdir(parents=True, exist_ok=True)
    with (ROOT / 'state.lock').open('a+b') as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        path = ROOT / 'state.json'
        state = validate(json.loads(path.read_bytes())) if read and path.exists() else {'version': 1, 'records': {}}
        yield state

def save(state):
    raw = encode(validate(state))
    fd, tmp = tempfile.mkstemp(prefix='.state-', dir=ROOT)
    try:
        with os.fdopen(fd, 'wb') as f:
            f.write(raw); f.flush(); os.fsync(f.fileno())
        os.replace(tmp, ROOT / 'state.json')
        fd = os.open(ROOT, os.O_RDONLY); os.fsync(fd); os.close(fd)
    finally:
        if os.path.exists(tmp): os.unlink(tmp)

def verify(state, request):
    key = request['key']
    if key not in state['records'] or encode(state['records'][key]) != encode(request['equals']):
        raise ValueError('fixture application verification failed')

def control(action):
    if action == 'version':
        print(VERSION); return
    request = json.load(sys.stdin)
    with locked(read=action != 'restore') as state:
        if action == 'restore':
            candidate = validate(request['snapshot'])
            verify(candidate, request)
            save(candidate)
            print(json.dumps({'ok': True}))
        elif action in ('snapshot', 'probe'):
            verify(state, request)
            sys.stdout.buffer.write(encode(state if action == 'snapshot' else {'ok': True}))
        else:
            raise ValueError('unknown fixture control operation')

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass

    def reply(self, status, value):
        raw = encode(value)
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers(); self.wfile.write(raw)

    def dispatch(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == '/health' and self.command == 'GET':
            with locked(): pass
            return self.reply(200, {'ready': True, 'version': VERSION})
        key = urllib.parse.unquote(path.removeprefix('/records/'))
        if not path.startswith('/records/') or not KEY.fullmatch(key):
            return self.reply(404, {'error': 'unknown fixture resource'})
        try:
            with locked() as state:
                if self.command == 'GET':
                    if key not in state['records']: return self.reply(404, {'error': 'record missing'})
                    return self.reply(200, state['records'][key])
                if self.command == 'PUT':
                    size = int(self.headers.get('Content-Length', '0'))
                    if size < 1 or size > 1024 * 1024: return self.reply(413, {'error': 'record exceeds bounds'})
                    value = json.loads(self.rfile.read(size))
                    state['records'][key] = value
                    save(state)
                    return self.reply(200, value)
                if self.command == 'DELETE':
                    state['records'].pop(key, None); save(state)
                    return self.reply(200, {'deleted': True})
        except (ValueError, TypeError):
            return self.reply(400, {'error': 'invalid fixture request'})
        self.reply(405, {'error': 'unsupported method'})

    do_GET = dispatch
    do_PUT = dispatch
    do_DELETE = dispatch

if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] != 'serve':
        control(sys.argv[1])
    else:
        http.server.ThreadingHTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
