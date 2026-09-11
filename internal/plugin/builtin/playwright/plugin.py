import hashlib
import json
import os
import pathlib
import subprocess
import sys
import urllib.parse

IMAGE = 'mcr.microsoft.com/playwright@sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27'
DOCKER = ['docker', '--host', 'unix:///var/run/docker.sock']

def docker(args, check=True):
    result = subprocess.run(DOCKER + args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if check and result.returncode:
        raise RuntimeError('Scoped browser container operation failed')
    return result

def inspect(kind, name):
    result = docker([kind, 'inspect', name], False)
    return json.loads(result.stdout)[0] if result.returncode == 0 else None

def atomic(path, value):
    tmp = path.with_suffix('.tmp')
    with tmp.open('wb') as f:
        f.write(value); f.flush(); os.fsync(f.fileno())
    os.replace(tmp, path)

def main(request):
    config = request['config']
    url = urllib.parse.urlsplit(config['base_url'])
    if url.scheme != 'http' or url.hostname not in ('localhost', '127.0.0.1', '::1') or url.username or url.password or url.query or url.fragment:
        raise RuntimeError('base_url must name the application on guest loopback HTTP')
    package = pathlib.Path(__file__).resolve().parent
    state = pathlib.Path(os.environ['ENVCTL_PLUGIN_STATE'])
    scope = hashlib.sha256(json.dumps([request['run_id'], request['revision'], config, 'playwright@1.0.0'], sort_keys=True).encode()).hexdigest()[:24]
    label = 'dev.envctl.plugin.scope=' + scope
    volume = 'envctl-browser-' + scope
    operation = request['operation']
    if operation == 'cleanup':
        owned = docker(['ps', '-aq', '--filter', 'label=' + label]).stdout.decode().split()
        for name in owned: docker(['rm', '-f', name])
        existing = inspect('volume', volume)
        if existing:
            if existing.get('Labels', {}).get('dev.envctl.plugin.scope') != scope: raise RuntimeError('Browser volume ownership differs')
            docker(['volume', 'rm', volume])
        return {'ok':True, 'detail':'Owned browser containers and tools removed'}
    if operation == 'prepare':
        if not inspect('image', IMAGE): docker(['pull', IMAGE])
        existing = inspect('volume', volume)
        if existing and existing.get('Labels', {}).get('dev.envctl.plugin.scope') != scope: raise RuntimeError('Browser volume ownership differs')
        if not existing: docker(['volume', 'create', '--label', label, volume])
    elif not inspect('volume', volume) or not inspect('image', IMAGE):
        response = {'ok':False, 'detail':'Prepared browser tools or image are missing'}
        if operation == 'probe': response['recovery'] = 'prepare'
        return response
    key = hashlib.sha256(request['operation_id'].encode()).hexdigest()[:24]
    name = volume + '-' + key
    payload = json.dumps({'operation':operation,'config':config,'input':request.get('input')}).encode()
    payload_path = state / (key + '.json')
    if payload_path.exists() and payload_path.read_bytes() != payload: raise RuntimeError('Browser operation input differs')
    if not payload_path.exists(): atomic(payload_path, payload)
    container = inspect('container', name)
    if container and container.get('Config', {}).get('Labels', {}).get('dev.envctl.plugin.scope') != scope: raise RuntimeError('Browser container ownership differs')
    if not container:
        args = ['create', '--name', name, '--label', label, '--init', '--network', 'host', '--shm-size', '256m', '--memory', '768m',
                '-e', 'NODE_PATH=/tools/node_modules', '-e', 'PLAYWRIGHT_BROWSERS_PATH=/ms-playwright',
                '-v', str(package)+':/package:ro', '-v', str(payload_path)+':/request.json:ro',
                '-v', volume+':/tools'+('' if operation == 'prepare' else ':ro'), IMAGE]
        if operation == 'prepare':
            args += ['sh','-c','cp /package/package*.json /tools/ && cd /tools && npm ci --ignore-scripts --omit=optional --no-audit --no-fund >/dev/null 2>&1 && node /package/browser.cjs']
        else: args += ['node', '/package/browser.cjs']
        docker(args)
        container = inspect('container', name)
    if container['State']['Status'] == 'created': docker(['start', name])
    docker(['wait', name])
    logs = docker(['logs', name]).stdout
    if len(logs) > 2*1024*1024: raise RuntimeError('Browser evidence exceeds its limit')
    try: result = json.loads(logs)
    except Exception: raise RuntimeError('Browser did not return structured execution evidence')
    return {'ok':result.get('ok') is True,'detail':'Chromium '+operation+' '+('passed' if result.get('ok') else 'failed'),'output':result}

request = {}
try:
    request = json.load(sys.stdin)
    response = main(request)
except Exception as error:
    response = {'ok':False,'detail':str(error)[:1000]}
response.update(protocol=1, expires_seconds=60 if request.get('operation') == 'probe' else 0)
print(json.dumps(response), flush=True)
