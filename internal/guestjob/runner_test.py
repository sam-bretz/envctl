import importlib.util
import io
import json
import os
import pathlib
import pwd
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('runner', pathlib.Path(__file__).with_name('runner.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        runner.ROOT = pathlib.Path(self.temp.name).resolve() / 'jobs'
        runner.WORK_ROOT = pathlib.Path(self.temp.name).resolve() / 'work'
        self.work = runner.WORK_ROOT / 'revision'
        self.work.mkdir(parents=True)

    def request(self, **overrides):
        return {'id': 'attempt_one', 'args': [sys.executable, '-c', 'print("done")'],
                'dir': str(self.work), 'env': {}, 'input': '', 'secrets': [],
                'timeout_seconds': 5, **overrides}

    def seed(self, req):
        directory = runner.jobdir(req['id'])
        directory.mkdir(parents=True)
        runner.atomic(directory / 'request.json', req)
        runner.atomic(directory / 'receipt.json', {'digest': 'test'})
        return directory

    def execute(self, req):
        user = pwd.getpwuid(os.getuid())
        popen = subprocess.Popen
        def unprivileged(*args, **kwargs):
            self.assertEqual(kwargs.pop('user'), user.pw_uid)
            self.assertEqual(kwargs.pop('group'), user.pw_gid)
            kwargs.pop('extra_groups')
            return popen(*args, **kwargs)
        with patch.object(runner.pwd, 'getpwnam', return_value=user), patch.object(runner.subprocess, 'Popen', side_effect=unprivileged):
            runner.run(req['id'])

    def test_durable_result_and_no_reexecution(self):
        req = self.request(args=[sys.executable, '-c', 'from pathlib import Path; p=Path("count"); p.write_text(p.read_text()+"x" if p.exists() else "x"); print("done")'])
        directory = self.seed(req)
        self.execute(req)
        self.execute(req)
        self.assertEqual((self.work / 'count').read_text(), 'x')
        result = runner.status({'id': req['id']})
        self.assertEqual(result['state'], 'completed')
        self.assertEqual(result['exit_code'], 0)
        self.assertEqual(result['output'], 'done\n')
        self.assertEqual(runner.status({'id': req['id'], 'cursor': result['cursor']})['output'], '')
        self.assertEqual((directory / 'result.json').stat().st_mode & 0o777, 0o600)

    def test_secret_across_every_possible_chunk_boundary(self):
        raw = b'hello token-that-must-not-leak goodbye token-that-must-not-leak!'
        for size in range(1, len(raw)):
            redactor = runner.Redactor(['token-that-must-not-leak'])
            result = b''.join(redactor.feed(raw[i:i+size]) for i in range(0, len(raw), size)) + redactor.feed(b'', final=True)
            self.assertEqual(result, b'hello [REDACTED] goodbye [REDACTED]!')

    def test_actual_output_redaction_and_timeout(self):
        req = self.request(args=[sys.executable, '-c', 'import time; print("private-token", flush=True); time.sleep(10)'], secrets=['private-token'], timeout_seconds=1)
        self.seed(req)
        self.execute(req)
        result = runner.status({'id': req['id']})
        self.assertEqual(result['state'], 'timed-out')
        self.assertEqual(result['output'], '[REDACTED]\n')

    def test_start_intent_replay_and_request_conflict(self):
        req = self.request()
        # Simulate transport loss after systemd accepted the start: the first
        # submit raises, but the second sees the retained unit and starts none.
        unit_state = [False]
        starts = []
        def ctl(*args):
            value = b'loaded' if unit_state[0] else b'not-found'
            if 'ActiveState' in ' '.join(args):
                value = b'active'
            return subprocess.CompletedProcess(args, 0, value, b'')
        def launch(args, **kwargs):
            starts.append(args)
            unit_state[0] = True
            raise TimeoutError('lost acknowledgement')
        with patch.object(runner, 'systemctl', side_effect=ctl), patch.object(runner.subprocess, 'run', side_effect=launch):
            with self.assertRaises(TimeoutError):
                runner.submit(req)
            self.assertEqual(runner.submit(req)['state'], 'starting')
            self.assertEqual(len(starts), 1)
            with self.assertRaisesRegex(ValueError, 'different request'):
                runner.submit({**req, 'input': 'changed'})

    def test_interrupted_execution_requires_new_attempt(self):
        req = self.request()
        directory = self.seed(req)
        runner.atomic(directory / 'started.json', {'at': 0})
        with patch.object(runner, 'systemctl', return_value=subprocess.CompletedProcess([], 0, b'inactive', b'')):
            self.assertEqual(runner.status({'id': req['id']})['state'], 'interrupted')
        self.execute(req)
        self.assertFalse((directory / 'result.json').exists())

    def test_path_escape_rejected(self):
        with self.assertRaises(ValueError):
            runner.jobdir('../outside')
        with self.assertRaisesRegex(ValueError, 'working directory'):
            runner.submit(self.request(dir=str(self.work / '..' / '..')))

    def test_output_cursor_preserves_partial_utf8_writes(self):
        req = self.request()
        directory = self.seed(req)
        runner.atomic(directory / 'result.json', {'state': 'completed', 'exit_code': 0})
        encoded = 'before 🌱 after'.encode()
        position = len('before '.encode()) + 2
        (directory / 'output.log').write_bytes(encoded[:position])
        first = runner.status({'id': req['id']})
        self.assertEqual(first['output'], 'before ')
        with (directory / 'output.log').open('ab') as out:
            out.write(encoded[position:])
        second = runner.status({'id': req['id'], 'cursor': first['cursor']})
        self.assertEqual(first['output'] + second['output'], 'before 🌱 after')


if __name__ == '__main__':
    unittest.main()
