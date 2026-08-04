"""Tests against the real Hermes install on this box (golden tests).

These assert against live data — on a host without Hermes they skip.
"""

import os

import pytest

from sloppy_toppy import discovery


def _installs():
    return discovery.find_hermes_installs()


def test_discovers_at_least_one_install():
    installs = _installs()
    if not installs:
        pytest.skip("no live Hermes installs on this host")
    assert len(installs) >= 1


def test_each_install_has_readable_state_db():
    installs = _installs()
    if not installs:
        pytest.skip("no live Hermes installs on this host")
    for inst in installs:
        assert os.path.exists(inst.state_db)
        assert inst.gateway_pid is not None
        assert os.path.exists(f"/proc/{inst.gateway_pid}"), "gateway must be alive"


def test_slack_profile_is_running():
    installs = _installs()
    if not installs:
        pytest.skip("no live Hermes installs on this host")
    profiles = {i.profile for i in installs}
    assert "slack" in profiles, f"expected slack profile among {profiles}"
