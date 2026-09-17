"""mitmproxy addon: write HAR files named after CAPTURE_SCENARIO."""

from __future__ import annotations

import os
import time
from pathlib import Path

from mitmproxy import ctx, http
from mitmproxy.addons.har_dump import HARDump


class ScenarioHAR:
    def __init__(self) -> None:
        self._dump = HARDump()

    def load(self, loader) -> None:
        loader.add_option(
            name="capture_dir",
            typespec=str,
            default="/captures",
            help="Directory for HAR output",
        )

    def running(self) -> None:
        scenario = os.environ.get("CAPTURE_SCENARIO", "untitled")
        ts = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
        out = Path(ctx.options.capture_dir) / f"{scenario}-{ts}.har"
        out.parent.mkdir(parents=True, exist_ok=True)
        ctx.options.hardump = str(out)
        ctx.log.info(f"writing HAR to {out}")


addons = [ScenarioHAR()]
