"""プロファイルTOMLの読み込み・デフォルト値・検証（SPEC §3）"""

from __future__ import annotations

import tomllib
from dataclasses import dataclass, field, fields
from pathlib import Path


class ConfigError(Exception):
    pass


@dataclass
class Limits:
    max_workers: int = 10
    articles_per_feed: int = 10
    select_max_articles: int = 2000
    score_chunk_size: int = 50
    score_workers: int = 5
    summary_workers: int = 5
    html_max_chars: int = 200_000
    text_max_chars: int = 20_000
    text_min_chars: int = 300
    max_failures: int = 5
    failure_retry_hours: int = 168
    posted_retention_days: int = 14
    embeds_per_message: int = 10


@dataclass
class Config:
    name: str
    opml_path: str
    webhook_env: str
    select_criteria: str
    hours_back: int = 24
    pick_count: int = 20
    embed_color: int = 0x2ECC71
    header_title: str = ""
    user_memory_path: str = ""
    user_memory: str = ""
    model: str = ""
    cache_path: str = ""
    limits: Limits = field(default_factory=Limits)


_TOP_LEVEL_KEYS = {
    "name", "opml_path", "webhook_env", "hours_back", "pick_count",
    "embed_color", "header_title", "user_memory_path", "model", "cache_path",
    "prompts", "limits",
}


def load_config(path: str | Path) -> Config:
    path = Path(path)
    try:
        with open(path, "rb") as f:
            raw = tomllib.load(f)
    except FileNotFoundError:
        raise ConfigError(f"プロファイルが見つかりません: {path}")
    except tomllib.TOMLDecodeError as e:
        raise ConfigError(f"TOMLの構文エラー: {path}: {e}")

    unknown = set(raw) - _TOP_LEVEL_KEYS
    if unknown:
        raise ConfigError(f"未知の設定キー: {', '.join(sorted(unknown))}")

    for key in ("name", "opml_path", "webhook_env"):
        if not raw.get(key):
            raise ConfigError(f"必須キーがありません: {key}")

    prompts = raw.get("prompts", {})
    select_criteria = prompts.get("select_criteria", "")
    if not select_criteria.strip():
        raise ConfigError("prompts.select_criteria は必須です")
    unknown = set(prompts) - {"select_criteria"}
    if unknown:
        raise ConfigError(f"未知の[prompts]キー: {', '.join(sorted(unknown))}")

    limits_raw = raw.get("limits", {})
    limit_names = {f.name for f in fields(Limits)}
    unknown = set(limits_raw) - limit_names
    if unknown:
        raise ConfigError(f"未知の[limits]キー: {', '.join(sorted(unknown))}")
    for key, value in limits_raw.items():
        if not isinstance(value, int) or value <= 0:
            raise ConfigError(f"limits.{key} は正の整数を指定してください: {value!r}")
    limits = Limits(**limits_raw)

    name = raw["name"]
    cfg = Config(
        name=name,
        opml_path=raw["opml_path"],
        webhook_env=raw["webhook_env"],
        select_criteria=select_criteria,
        hours_back=raw.get("hours_back", 24),
        pick_count=raw.get("pick_count", 20),
        embed_color=raw.get("embed_color", 0x2ECC71),
        header_title=raw.get("header_title") or name,
        user_memory_path=raw.get("user_memory_path", ""),
        model=raw.get("model", ""),
        cache_path=raw.get("cache_path") or f".cache/{name}.json",
        limits=limits,
    )

    # user_memoryは任意。ファイルが存在しない場合も汎用基準で動く（SPEC §3）
    if cfg.user_memory_path:
        p = Path(cfg.user_memory_path)
        if p.is_file():
            cfg.user_memory = p.read_text(encoding="utf-8").strip()

    return cfg
