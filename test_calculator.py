import unittest
from calculator import add
class AdditionTests(unittest.TestCase):
    def test_addition(self):
        for a, b in [(2,3),(-4,1),(0,0)]:
            self.assertEqual(add(a,b),a+b)
