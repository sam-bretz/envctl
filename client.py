import json
import urllib.request


def remote_add(base_url, a, b):
    payload = json.dumps({'a': a, 'b': b}).encode('utf-8')
    request = urllib.request.Request(
        base_url,
        data=payload,
        headers={'Content-Type': 'application/json'},
        method='POST',
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.loads(response.read().decode('utf-8'))['result']
