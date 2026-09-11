import unittest
from client import remote_add
class ClientTests(unittest.TestCase):
    def test_http_addition(self):
        for a, b in [(2,3),(-4,1),(0,0)]:
            self.assertEqual(remote_add('http://127.0.0.1:18088',a,b), a+b)
