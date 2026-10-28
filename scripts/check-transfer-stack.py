#!/usr/bin/env python3
"""Run the disposable media stack check with the opt-in mock transfer sidecar."""
import importlib.util
from pathlib import Path


def main():
    path = Path(__file__).with_name("check-media-selection-stack.py")
    spec = importlib.util.spec_from_file_location("arknet_media_stack_check", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.main(with_transfer=True)


if __name__ == "__main__":
    main()
