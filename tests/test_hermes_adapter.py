"""Golden tests: the Hermes adapter must parse the real live state.db."""

import pytest

from sloppy_toppy import discovery, hermes

# This box's live session id (this very conversation). Assertions on token
# counts are intentionally loose — counters grow while the test runs.
LIVE_SESSION = "20260804_174025_3fd0e781"


def _slack_install():
    for inst in discovery.find_hermes_installs():
        if inst.profile == "slack":
            return inst
    pytest.skip("slack profile not running on this host")


def test_parses_live_session():
    inst = _slack_install()
    agents = hermes.read_sessions(inst)
    live = [a for a in agents if a.session_id == LIVE_SESSION]
    assert len(live) == 1, "live session must be present"
    a = live[0]
    assert a.source == "hermes:slack"
    assert a.model == "deepseek/deepseek-v4-flash-0731"
    assert a.input_tokens > 1000, "input counter must be a real int"
    assert a.output_tokens >= 0 and a.reasoning_tokens >= 0
    assert a.ended_at is None, "live session must not be ended"
    assert a.started_at is not None and a.started_at > 1e9
    assert a.last_activity_at is not None
    assert a.cost_usd >= 0.0


def test_context_window_from_cache():
    inst = _slack_install()
    cache = hermes.load_context_cache(inst.home)
    assert cache, "context_length_cache.yaml must parse"
    win = hermes.model_ctx_window("deepseek/deepseek-v4-flash-0731", cache)
    assert win == 1_048_576, "deepseek v4 window from the real cache"


def test_unknown_model_falls_back_to_default():
    assert hermes.model_ctx_window("totally/fake-model", {}) == 128_000
    cache = {"totally/fake-model@http://example.invalid": 4096}
    assert hermes.model_ctx_window("totally/fake-model", cache) == 4096


def test_all_sessions_parse_without_error():
    inst = _slack_install()
    agents = hermes.read_sessions(inst)
    assert agents, "slack profile must have sessions"
    for a in agents:
        assert isinstance(a.input_tokens, int)
        assert isinstance(a.cost_usd, float)
        assert a.ctx_window > 0
