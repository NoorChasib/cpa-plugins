#!/usr/bin/env python3
"""Stream Go test output, requiring the actual browser test to run and pass once."""
import json
import sys

PACKAGE = "github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/plugin"
TEST = "TestSidebarBrowser"


def require_browser_pass(lines, output=sys.stdout):
    test_actions = []
    package_actions = []
    for line in lines:
        event = json.loads(line)
        if "Output" in event:
            output.write(event["Output"])
            output.flush()
        if event.get("Package") != PACKAGE:
            continue
        action = event.get("Action")
        if event.get("Test") == TEST and action in ("run", "pass", "skip", "fail"):
            test_actions.append(action)
        if "Test" not in event and action in ("pass", "skip", "fail"):
            package_actions.append(action)
    if test_actions != ["run", "pass"] or package_actions != ["pass"]:
        raise ValueError(f"mandatory {TEST} did not run and pass exactly once: test={test_actions}, package={package_actions}")


if __name__ == "__main__":
    try:
        require_browser_pass(sys.stdin)
    except (ValueError, KeyError, TypeError) as error:
        raise SystemExit(str(error))
