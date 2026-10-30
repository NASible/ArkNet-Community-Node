"""Unit guards for the live HTTPS browser acceptance harness."""
import importlib.util
from pathlib import Path
import unittest

PATH = Path(__file__).with_name('check-operator-https.py')


class HarnessTests(unittest.TestCase):
    def test_cookie_and_storage_guards(self):
        self.assertTrue(PATH.exists(), 'HTTPS acceptance harness is missing')
        spec = importlib.util.spec_from_file_location('https_check', PATH)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        good = {'cookie': {'secure': True, 'httpOnly': True, 'sameSite': 'Strict', 'path': '/'},
                'local': 0, 'session': 0, 'visibleCookie': False}
        module.validate_browser_state(good)
        for key in ('secure', 'httpOnly', 'sameSite', 'path'):
            bad = {**good, 'cookie': {**good['cookie'], key: None}}
            with self.subTest(key=key), self.assertRaises(AssertionError):
                module.validate_browser_state(bad)
        for key in ('local', 'session', 'visibleCookie'):
            with self.subTest(key=key), self.assertRaises(AssertionError):
                module.validate_browser_state({**good, key: 1})


if __name__ == '__main__':
    unittest.main()
