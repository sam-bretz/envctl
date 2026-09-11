import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from calculator import add
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if self.path == '/health' else 404)
        self.end_headers()
    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        try:
            result = {'result': add(data['a'], data['b'])}
            self.send_response(200)
        except Exception:
            result = {'error': 'not implemented'}
            self.send_response(501)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps(result).encode())
HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
