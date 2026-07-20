"""Claude CLI呼び出し・プロンプトテンプレート・usage集計（SPEC §9）"""

from __future__ import annotations

import json
import logging
import subprocess
import threading
from collections import defaultdict

log = logging.getLogger(__name__)

# タイムアウト（SPEC §9.1）
SELECT_TIMEOUT = 300
SCORE_TIMEOUT = 120
SUMMARY_TIMEOUT = 90
FALLBACK_TIMEOUT = 120

# ツール制限（SPEC §9.1）。-pモードでは許可が必要なツールは自動拒否されるが、
# 読み取り系は権限なしで動くため、信頼できない本文を渡す呼び出しでは明示的に塞ぐ。
CONTENT_DISALLOWED_TOOLS = "WebFetch WebSearch Bash Read Glob Grep"
FALLBACK_ALLOWED_TOOLS = "WebFetch"
FALLBACK_DISALLOWED_TOOLS = "Bash Read Glob Grep"


class ClaudeError(Exception):
    pass


def run_claude(
    prompt: str,
    *,
    timeout: int,
    model: str = "",
    allowed_tools: str = "",
    disallowed_tools: str = "",
) -> tuple[str, dict | None]:
    """`claude -p` を実行し (生成テキスト, usage) を返す。

    プロンプトは必ずstdin経由で渡す（コマンドライン引数だとMAX_ARG_STRLEN超過で
    クラッシュする。SPEC 付録A.2）。
    """
    cmd = ["claude", "-p", "--output-format", "json"]
    if model:
        cmd += ["--model", model]
    if allowed_tools:
        cmd += ["--allowedTools", allowed_tools]
    if disallowed_tools:
        cmd += ["--disallowedTools", disallowed_tools]

    try:
        proc = subprocess.run(
            cmd,
            input=prompt,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        raise ClaudeError(f"タイムアウト（{timeout}秒）")
    except OSError as e:
        raise ClaudeError(f"claude CLIの起動に失敗: {e}")

    if proc.returncode != 0:
        stderr = (proc.stderr or "").strip()[-500:]
        raise ClaudeError(f"claude CLIが異常終了 (exit {proc.returncode}): {stderr}")

    try:
        data = json.loads(proc.stdout)
    except json.JSONDecodeError:
        # JSONパースに失敗した場合は生stdout全体を生成テキストとして扱う（SPEC §9.1）
        return proc.stdout, None

    result = data.get("result", "")
    if data.get("is_error"):
        raise ClaudeError(f"claudeがエラーを返却: {str(result)[:500]}")
    return result, data.get("usage")


class UsageTracker:
    """段階別のClaude呼び出し回数・トークン数の集計（SPEC §9.3）。スレッドセーフ。"""

    STAGES = ("select", "score", "summarize", "summarize_fallback")

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._stages: dict[str, dict[str, int]] = defaultdict(
            lambda: {"calls": 0, "input": 0, "output": 0,
                     "cache_read": 0, "cache_creation": 0, "unknown": 0}
        )

    def record(self, stage: str, usage: dict | None) -> None:
        with self._lock:
            s = self._stages[stage]
            s["calls"] += 1
            if usage is None:
                s["unknown"] += 1
                return
            s["input"] += usage.get("input_tokens", 0)
            s["output"] += usage.get("output_tokens", 0)
            s["cache_read"] += usage.get("cache_read_input_tokens", 0)
            s["cache_creation"] += usage.get("cache_creation_input_tokens", 0)

    def calls(self, stage: str) -> int:
        with self._lock:
            return self._stages[stage]["calls"] if stage in self._stages else 0

    def _totals(self) -> dict[str, int]:
        totals = {"calls": 0, "input": 0, "output": 0, "cache_read": 0,
                  "cache_creation": 0, "unknown": 0}
        for s in self._stages.values():
            for k in totals:
                totals[k] += s[k]
        return totals

    def summary_lines(self) -> list[str]:
        """標準出力向けの段階別サマリ。"""
        with self._lock:
            lines = ["Claude使用量サマリ:"]
            for stage in self.STAGES:
                if stage not in self._stages:
                    continue
                s = self._stages[stage]
                line = (
                    f"  {stage}: {s['calls']}回 / in {s['input']:,} tok"
                    f" (cache read {s['cache_read']:,}, creation {s['cache_creation']:,})"
                    f" / out {s['output']:,} tok"
                )
                if s["unknown"]:
                    line += f" / usage不明 {s['unknown']}回"
                lines.append(line)
            t = self._totals()
            lines.append(
                f"  合計: {t['calls']}回 / in {t['input']:,} tok"
                f" (cache read {t['cache_read']:,}, creation {t['cache_creation']:,})"
                f" / out {t['output']:,} tok"
            )
            return lines

    def discord_line(self) -> str:
        """Discord向けの1行サマリ（SPEC §10）。"""
        with self._lock:
            parts = []
            if "select" in self._stages:
                parts.append(f"選定{self._stages['select']['calls']}回")
            if "score" in self._stages:
                parts.append(f"スコアリング{self._stages['score']['calls']}回")
            if "summarize" in self._stages or "summarize_fallback" in self._stages:
                n = self._stages["summarize"]["calls"] if "summarize" in self._stages else 0
                fb = (
                    self._stages["summarize_fallback"]["calls"]
                    if "summarize_fallback" in self._stages
                    else 0
                )
                s = f"要約{n + fb}回"
                if fb:
                    s += f"(救済{fb}回)"
                parts.append(s)
            t = self._totals()
            return (
                f"-# Claude使用量: {'・'.join(parts) or '呼び出しなし'}"
                f" / in {t['input']:,} tok (cache {t['cache_read']:,})"
                f" / out {t['output']:,} tok"
            )


# --- プロンプトテンプレート（SPEC §9.2） ---
# テンプレート中にJSON例の波括弧が含まれるため、str.formatではなく.replaceで埋める

_USER_CONTEXT_HEADER = "# ユーザーの興味・趣向（この情報を採点基準に加味してください）"

_SELECT_TEMPLATE = """\
以下は複数のRSSフィードから集めた最近の記事のタイトル一覧です。{user_context}
この中から特に注目すべき記事を{pick_count}件選んでください。

# 記事一覧
{articles}

# 選定基準
{select_criteria}

# 出力形式（JSONのみ、前置き・後書き・コードブロック記号は不要）
選んだ記事の[index]を配列で出力してください。
[0, 5, 12, ...]
"""

_SCORE_TEMPLATE = """\
以下の記事リストについて、各記事の注目度を0〜100でスコアリングしてください。{user_context}

# 評価基準
{select_criteria}

# 記事リスト
{articles}

# 出力形式（JSONのみ、前置き・後書き・コードブロック記号は不要）
[
  {"index": 0, "score": 85},
  {"index": 1, "score": 42},
  ...
]
"""

_SUMMARY_TEMPLATE = """\
以下はニュース記事のタイトルと、そのページのHTMLからタグを機械的に除去して抽出したテキストです
（ナビゲーション等の本文以外のテキストも混ざっています）。
この記事の内容を日本語で2〜3文に要約してください。なぜ注目すべきかも一言添えてください。
記事タイトルが日本語以外の場合も、要約は日本語で書いてください。

# タイトル
{title}

# ページテキスト
{text}

# 出力
要約文のみを出力してください。前置き・後書き・タイトルの再掲・Markdown装飾（**太字**など）は不要です。
"""

_FALLBACK_TEMPLATE = """\
以下はRSS記事のタイトルとURLです。Python側での本文取得が403エラーやアクセス制限などで失敗したか、
ページからテキストがほぼ抽出できませんでした。
WebFetchツールが使えるなら、このURLに直接アクセスできないか試してから要約してください。
アクセスできない・許可が下りない場合は、タイトルから読み取れる範囲で日本語1〜2文の
短い要約・注目ポイントを書いてください（憶測で事実を断定しないでください）。

# タイトル
{title}

# URL
{url}

# 出力
要約文のみを出力してください。前置き・後書きは不要です。
"""


def format_user_context(user_memory: str) -> str:
    if not user_memory.strip():
        return ""
    return f"\n\n{_USER_CONTEXT_HEADER}\n{user_memory.strip()}\n"


def build_select_prompt(
    articles: str, pick_count: int, select_criteria: str, user_memory: str
) -> str:
    return (
        _SELECT_TEMPLATE
        .replace("{user_context}", format_user_context(user_memory))
        .replace("{pick_count}", str(pick_count))
        .replace("{articles}", articles)
        .replace("{select_criteria}", select_criteria.strip())
    )


def build_score_prompt(articles: str, select_criteria: str, user_memory: str) -> str:
    return (
        _SCORE_TEMPLATE
        .replace("{user_context}", format_user_context(user_memory))
        .replace("{articles}", articles)
        .replace("{select_criteria}", select_criteria.strip())
    )


def build_summary_prompt(title: str, text: str) -> str:
    return _SUMMARY_TEMPLATE.replace("{title}", title).replace("{text}", text)


def build_fallback_prompt(title: str, url: str) -> str:
    return _FALLBACK_TEMPLATE.replace("{title}", title).replace("{url}", url)
