"""Version 1 guest job protocol. Standard library only; invoked by root.

Requests (including scoped credentials) stay in a root-private directory.
systemd, not an SSH session, owns job lifetime. A fixed unit name and a per-job
flock close the submit/reconnect race. Interrupted execution is never silently
replayed; the coordinator must create a new attempt or resume the harness.
"""
import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import pwd
import re
import selectors
import signal
import subprocess
import sys
import tempfile
import time

ROOT = pathlib.Path('/var/lib/envctl/jobs')
WORK_ROOT = pathlib.Path('/work/envctl')
MAX_LOG = 16 * 1024 * 1024
MAX_REQUEST = 4 * 1024 * 1024


def atomic(path, value):
    fd, temp = tempfile.mkstemp(prefix='.write-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as out:
            json.dump(value, out, separators=(',', ':'))
            out.flush()
            os.fsync(out.fileno())
        os.replace(temp, path)
        fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def read(path):
    with path.open() as src:
        return json.load(src)


def jobdir(job_id):
    if not isinstance(job_id, str) or not re.fullmatch(r'[a-z][a-z0-9_-]{0,100}', job_id):
        raise ValueError('invalid job ID')
    return ROOT / job_id


def unit(job_id):
    return 'envctl-job-' + job_id + '.service'


def systemctl(*args):
    return subprocess.run(['systemctl', *args], capture_output=True, timeout=30)


@contextlib.contextmanager
def locked(directory):
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (directory / 'lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def submit(req):
    directory = jobdir(req['id'])
    if set(req) != {'id', 'args', 'dir', 'env', 'input', 'secrets', 'timeout_seconds'}:
        raise ValueError('unknown or missing request fields')
    if not isinstance(req['args'], list) or not req['args'] or any(not isinstance(x, str) or '\0' in x for x in req['args']):
        raise ValueError('invalid argument vector')
    work = pathlib.Path(req['dir']).resolve()
    if not work.is_relative_to(WORK_ROOT) or work == WORK_ROOT:
        raise ValueError('working directory must be inside the revision workspace')
    if not isinstance(req['timeout_seconds'], int) or not 1 <= req['timeout_seconds'] <= 86400:
        raise ValueError('invalid job timeout')
    if not isinstance(req['env'], dict) or any(not re.fullmatch('[A-Za-z_][A-Za-z0-9_]*', k) or not isinstance(v, str) or '\0' in v for k, v in req['env'].items()):
        raise ValueError('invalid environment')
    if not isinstance(req['input'], str) or not isinstance(req['secrets'], list) or any(not isinstance(x, str) for x in req['secrets']):
        raise ValueError('invalid input or redactions')
    digest = hashlib.sha256(json.dumps(req, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    with locked(directory):
        receipt = directory / 'receipt.json'
        if receipt.exists():
            if read(receipt)['digest'] != digest:
                raise ValueError('job ID reused with different request')
        else:
            # Request is durable before publishing intent. A crash here is safe:
            # no unit can run without a receipt.
            atomic(directory / 'request.json', req)
            atomic(receipt, {'digest': digest})
        if not (directory / 'started.json').exists() and not (directory / 'result.json').exists():
            # The deterministic unit name fences duplicate starts even if the
            # transport disappears before systemd-run returns. Retain units.
            loaded = systemctl('show', unit(req['id']), '--property=LoadState', '--value')
            if loaded.stdout.strip() in (b'', b'not-found'):
                result = subprocess.run(['systemd-run', '--quiet', '--unit=' + unit(req['id']),
                                         '--property=Type=exec', '--property=RemainAfterExit=yes',
                                         '--property=KillMode=control-group',
                                         sys.executable, str(pathlib.Path(__file__).resolve()), 'run', req['id']],
                                        capture_output=True, timeout=30)
                if result.returncode:
                    raise ValueError('systemd could not start guest job; retry submission to reconcile')
    return status({'id': req['id'], 'cursor': 0})


def reconcile(req):
    directory = jobdir(req['id'])
    if not (directory / 'receipt.json').exists():
        return status(req)
    return submit(read(directory / 'request.json'))


def status(req):
    directory = jobdir(req['id'])
    if not (directory / 'receipt.json').exists():
        return {'id': req['id'], 'state': 'missing', 'cursor': 0, 'truncated': False}
    result = directory / 'result.json'
    if result.exists():
        value = read(result)
    else:
        state = systemctl('show', unit(req['id']), '--property=ActiveState', '--value').stdout.strip()
        value = {'state': 'running' if state in (b'active', b'activating', b'deactivating') else 'interrupted'}
        if not (directory / 'started.json').exists():
            loaded = systemctl('show', unit(req['id']), '--property=LoadState', '--value').stdout.strip()
            value['state'] = ('starting' if state in (b'active', b'activating') else
                              ('pending' if loaded in (b'', b'not-found') else 'interrupted'))
    cursor = req.get('cursor', 0)
    if not isinstance(cursor, int) or cursor < 0:
        raise ValueError('invalid output cursor')
    output = b''
    path = directory / 'output.log'
    if path.exists():
        with path.open('rb') as source:
            cursor = min(cursor, os.fstat(source.fileno()).st_size)
            source.seek(cursor)
            output = source.read(64 * 1024)
            # Keep byte cursors at UTF-8 boundaries, so clients can concatenate
            # pages without splitting multi-byte terminal output.
            try:
                output.decode('utf-8')
            except UnicodeDecodeError as error:
                if error.reason == 'unexpected end of data':
                    output = output[:error.start]
    return {**value, 'id': req['id'], 'output': output.decode('utf-8', errors='replace'),
            'cursor': cursor + len(output), 'truncated': (directory / 'truncated').exists()}


class Redactor:
    """Keep an overlap so secrets split across pipe reads cannot escape."""
    def __init__(self, secrets):
        self.secrets = sorted({s.encode() for s in secrets if s}, key=len, reverse=True)
        self.overlap = max((len(s) for s in self.secrets), default=1) - 1
        self.pending = b''

    def feed(self, data, final=False):
        self.pending += data
        safe = len(self.pending) if final else max(0, len(self.pending) - self.overlap)
        # A match crossing the boundary must be consumed whole, otherwise its
        # suffix could be emitted unredacted on the next read.
        out = bytearray()
        i = 0
        while i < safe:
            found = next((s for s in self.secrets if self.pending.startswith(s, i)), None)
            if found:
                out.extend(b'[REDACTED]')
                i += len(found)
            else:
                out.append(self.pending[i])
                i += 1
        self.pending = self.pending[i:]
        return bytes(out)


def run(job_id):
    directory = jobdir(job_id)
    # This independent lock protects against accidental manual launches too.
    with (directory / 'execution.lock').open('a') as execution:
        fcntl.flock(execution, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if (directory / 'started.json').exists() or (directory / 'result.json').exists():
            return
        req = read(directory / 'request.json')
        atomic(directory / 'started.json', {'at': time.time()})
        user = pwd.getpwnam('envctl-agent')
        env = {'PATH': '/usr/local/bin:/usr/bin:/bin', 'HOME': user.pw_dir, 'LANG': 'C.UTF-8', **req['env']}
        redactor = Redactor(req['secrets'])
        deadline = time.monotonic() + req['timeout_seconds']
        child = None
        exit_code = 127
        state = 'failed'
        detail = 'guest process could not start'
        try:
            # A file avoids blocking on a full stdin pipe before output draining.
            with tempfile.TemporaryFile() as stdin, (directory / 'output.log').open('wb', buffering=0) as out:
                stdin.write(req['input'].encode())
                stdin.seek(0)
                child = subprocess.Popen(req['args'], cwd=req['dir'], env=env, stdin=stdin,
                                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                         user=user.pw_uid, group=user.pw_gid,
                                         extra_groups=os.getgrouplist(user.pw_name, user.pw_gid), start_new_session=True)
                selector = selectors.DefaultSelector()
                selector.register(child.stdout, selectors.EVENT_READ)
                size = 0

                def emit(data):
                    nonlocal size
                    if size + len(data) > MAX_LOG:
                        (directory / 'truncated').touch(mode=0o600)
                    chunk = data[:max(0, MAX_LOG - size)]
                    out.write(chunk)
                    size += len(chunk)

                timed_out = False
                while selector.get_map():
                    if not timed_out and time.monotonic() >= deadline:
                        timed_out = True
                        try:
                            os.killpg(child.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    for key, _ in selector.select(timeout=0.2):
                        data = os.read(key.fd, 8192)
                        if not data:
                            selector.unregister(key.fileobj)
                        else:
                            emit(redactor.feed(data))
                emit(redactor.feed(b'', final=True))
                os.fsync(out.fileno())
                exit_code = child.wait()
                child.stdout.close()
                selector.close()
                state = 'timed-out' if timed_out else ('completed' if exit_code == 0 else 'failed')
                detail = 'time budget exhausted' if timed_out else ''
        finally:
            if child is not None:
                # Descendants may have closed stdout while continuing to run.
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            atomic(directory / 'result.json', {'state': state, 'exit_code': exit_code, 'detail': detail})


def cancel(req):
    directory = jobdir(req['id'])
    with locked(directory):
        if not (directory / 'receipt.json').exists():
            raise ValueError('cannot cancel an unknown job')
        if not (directory / 'result.json').exists():
            result = systemctl('stop', unit(req['id']))
            if result.returncode:
                raise ValueError('guest job cancellation failed')
            atomic(directory / 'result.json', {'state': 'cancelled', 'exit_code': -15})
    return status(req)


def main():
    os.umask(0o077)
    if len(sys.argv) == 3 and sys.argv[1] == 'run':
        run(sys.argv[2])
        return
    try:
        raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
        if len(raw) > MAX_REQUEST:
            raise ValueError('request too large')
        req = json.loads(raw)
        fn = {'submit': submit, 'status': status, 'cancel': cancel, 'reconcile': reconcile}.get(sys.argv[1])
        if fn is None:
            raise ValueError('unknown operation')
        response = fn(req)
    except ValueError as error:
        # Only explicitly constructed protocol errors are safe to return.
        response = {'state': 'error', 'detail': str(error) if type(error) is ValueError else 'invalid request', 'cursor': 0, 'truncated': False}
    except Exception:
        response = {'state': 'error', 'detail': 'guest job operation failed', 'cursor': 0, 'truncated': False}
    print(json.dumps(response))


if __name__ == '__main__':
    main()
