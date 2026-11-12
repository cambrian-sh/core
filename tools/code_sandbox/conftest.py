"""pytest setup for the code-sandbox server tests (server_test.py)."""


def pytest_configure(config):
    config.addinivalue_line("markers", "docker: needs a running Docker engine and the built sandbox image")
