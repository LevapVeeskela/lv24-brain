#!/usr/bin/env python3
"""One-off, reproducible scrub of e-mail addresses in lv24-brain raw zones.

The replacement is byte-for-byte the ingest placeholder of
internal/app/ingestion_policy.go, so a scrubbed value is indistinguishable from
one a new ingest would write:

    emailPattern     = [A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)*\\.[A-Za-z]{2,}
    EmailPlaceholder = "[email:" + hex(sha256(connectionID + "\\x00" + lower(address)))[:12] + "]"

Already-hashed "[email:<12 hex>]" tokens are matched first and left untouched,
so the scrub is idempotent. Only matched tokens are rewritten: files are handled
as bytes, line endings and every other byte stay as they are.

Raw zones (--include, default runs,items,dumps):
    items  connectors/<connectionID>/items/**   (raw-zone items)
    runs   connectors/runs/**                   (connector run logs)
    dumps  mail/**, github/**, gitlab/**        (legacy Markdown dumps)

The hash is connection-scoped, so every file needs a connection ID:
    items  the connector directory name, which is the connection ID
    dumps  mail/<account> -> the imap connection of that account (else
           "imap-<account>"); github/, gitlab/ -> the only connection of that
           provider (else the root name)
    runs   the run's "source:" provider -> the only connection of that provider
           (else the provider name; several imap connections share "imap")

Item files carry a content hash in their frontmatter
(domain.Item.ComputeHash: "sha256:" over item-v1, kind, collapsed title,
normalized body, URL, collapsed author, sorted labels and ACL, provider times,
project, deleted flag and state). Ingestion validates it and compares it with
the stored one to skip unchanged items, so scrub-raw-pii also recomputes it for
item files, with the same byte-exact encoding as the engine. It scrubs exactly
the fields the ingest policy cleans (title, body, URL, author, labels), so the
result equals what a fresh ingest of the scrubbed content would write. A file
whose item frontmatter does not parse is left untouched and counted in
unparsed.

Usage:
    python scripts/scrub-raw-pii.py <data-dir> [--apply] [--include runs,items,dumps]
    python scripts/scrub-raw-pii.py --self-test
    go run scripts/scrub-raw-pii-hash-check.go <data-dir>   # engine-side check

Without --apply nothing is written; it prints counts per top-level connector
directory and up to three example paths (values redacted).
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
import re
import shutil
import sys
import tempfile
from dataclasses import dataclass, field
from pathlib import Path

ZONES = ("runs", "items", "dumps")
DUMP_ROOTS = ("mail", "github", "gitlab")
SKIP_DIRS = {".git", ".locks"}

# Byte-oriented copy of emailPattern; the character classes are ASCII-only.
EMAIL_RE = re.compile(
    rb"\[email:[0-9a-f]{12}\]"
    rb"|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}"
)
# Same pattern for display strings (paths), which are decoded text.
PATH_EMAIL_RE = re.compile(
    r"\[email:[0-9a-f]{12}\]"
    r"|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}"
)
# Text-mode twin, for item fields decoded as str.
EMAIL_TEXT_RE = re.compile(
    r"\[email:[0-9a-f]{12}\]"
    r"|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}"
)
RUN_SOURCE_RE = re.compile(rb"^source:[ \t]*[\"']?([A-Za-z0-9._-]+)", re.MULTILINE)
RUN_NAME_RE = re.compile(r"^[0-9A-Za-z]+-(?P<provider>[A-Za-z0-9]+)-run-")

ITEM_HASH_PREFIX = "sha256:"
# The fields domain.itemTextFields hands to ingestPolicy.cleanText: the scrub
# touches exactly these, so the refreshed hash matches a fresh ingest. ACL and
# external_id are not cleaned by the policy and stay as they are.
ITEM_SCRUB_KEYS = ("title", "url", "author", "labels")
ITEM_LIST_KEYS = ("labels", "acl")
DELIMITER = "---"

# Self-test fixture and the hash the engine writes for it after the scrub,
# produced by `go run scripts/scrub-raw-pii-hash-check.go -print-hash <fixture>`
# on the scrubbed copy of scripts/scrub-raw-pii-fixture/item.md.
FIXTURE = Path(__file__).with_name("scrub-raw-pii-fixture") / "item.md"
FIXTURE_SCRUBBED_HASH = "sha256:643eb602de477d0f60d4bd1a5bc98176812800b959b5fb24e678e48319b4fefc"


def placeholder(connection_id: str, address: bytes) -> bytes:
    """Ingest placeholder of one address: 12 hex of sha256(id NUL lower(addr))."""
    digest = hashlib.sha256(
        connection_id.encode("utf-8") + b"\x00" + address.lower()
    ).hexdigest()
    return b"[email:" + digest[:12].encode("ascii") + b"]"


def scrub_bytes(data: bytes, connection_id: str) -> tuple[bytes, int, int]:
    """Replace raw addresses, keep existing placeholders.

    Returns (new data, replacements, placeholders seen).
    """
    replaced = 0
    hashed = 0

    def replace(match: re.Match[bytes]) -> bytes:
        nonlocal replaced, hashed
        token = match.group(0)
        if token.startswith(b"[email:"):
            hashed += 1
            return token
        replaced += 1
        return placeholder(connection_id, token)

    return EMAIL_RE.sub(replace, data), replaced, hashed


class ItemParseError(ValueError):
    """The file is not an item document this script can hash."""


_DOUBLE_ESCAPES = {
    "0": "\x00",
    "a": "\x07",
    "b": "\b",
    "t": "\t",
    "\t": "\t",
    "n": "\n",
    "v": "\x0b",
    "f": "\f",
    "r": "\r",
    "e": "\x1b",
    " ": " ",
    '"': '"',
    "/": "/",
    "\\": "\\",
    "N": "\x85",
    "_": "\xa0",
    "L": "\u2028",
    "P": "\u2029",
}
_HEX_ESCAPE_WIDTH = {"x": 2, "u": 4, "U": 8}
_KEY_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_-]*):(?:[ \t]*(.*))?$")
_TIME_RE = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2}):(\d{2})(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$"
)


def _decode_double(text: str, start: int) -> str:
    """Decode a YAML double-quoted scalar starting at text[start] == '"'."""
    out: list[str] = []
    index = start + 1
    while index < len(text):
        char = text[index]
        if char == '"':
            return "".join(out)
        if char == "\\":
            escape = text[index + 1] if index + 1 < len(text) else ""
            if escape in _DOUBLE_ESCAPES:
                out.append(_DOUBLE_ESCAPES[escape])
                index += 2
                continue
            if escape in _HEX_ESCAPE_WIDTH:
                width = _HEX_ESCAPE_WIDTH[escape]
                digits = text[index + 2:index + 2 + width]
                if len(digits) != width or any(c not in "0123456789abcdefABCDEF" for c in digits):
                    raise ItemParseError("bad YAML escape")
                out.append(chr(int(digits, 16)))
                index += 2 + width
                continue
            if escape == "\n":
                index += 2
                while index < len(text) and text[index] in " \t":
                    index += 1
                continue
            raise ItemParseError(f"unsupported YAML escape \\{escape}")
        if char == "\n":
            breaks = 0
            while index + breaks < len(text) and text[index + breaks] == "\n":
                breaks += 1
            index += breaks
            while index < len(text) and text[index] in " \t":
                index += 1
            out.append(" " if breaks == 1 else "\n" * (breaks - 1))
            continue
        out.append(char)
        index += 1
    raise ItemParseError("unterminated YAML double-quoted scalar")


def _decode_scalar(raw: str) -> str:
    text = raw.strip()
    if not text:
        return ""
    if text[0] == '"':
        return _decode_double(text, 0)
    if text[0] == "'":
        out: list[str] = []
        index = 1
        while index < len(text):
            if text[index] == "'":
                if index + 1 < len(text) and text[index + 1] == "'":
                    out.append("'")
                    index += 2
                    continue
                return "".join(out)
            out.append(text[index])
            index += 1
        raise ItemParseError("unterminated YAML single-quoted scalar")
    return text


def _entries(header: list[str]) -> list[tuple[str, list[int]]]:
    """Top-level frontmatter keys with the line indices that belong to them."""
    entries: list[tuple[str, list[int]]] = []
    for index, line in enumerate(header):
        match = _KEY_RE.match(line)
        if match:
            entries.append((match.group(1), [index]))
        elif entries:
            entries[-1][1].append(index)
    return entries


def _entry_scalar(header: list[str], lines: list[int]) -> str:
    first = header[lines[0]]
    text = first.split(":", 1)[1]
    if len(lines) > 1:
        text += "\n" + "\n".join(header[index] for index in lines[1:])
    return _decode_scalar(text)


def _entry_sequence(header: list[str], lines: list[int]) -> list[str]:
    tail = header[lines[0]].split(":", 1)[1].strip()
    if tail == "[]":
        return []
    if tail:
        raise ItemParseError("inline YAML sequence")
    values: list[str] = []
    for index in lines[1:]:
        item = header[index].strip()
        if not item:
            continue
        if not item.startswith("- "):
            raise ItemParseError("bad YAML sequence item")
        values.append(_decode_scalar(item[1:]))
    return values


@dataclass
class ItemDocument:
    lines: list[str]
    end: int
    header: list[str]
    body: str
    fields: dict[str, object]
    entries: list[tuple[str, list[int]]]

    @property
    def text(self) -> str:
        return "\n".join(self.lines)


def parse_item(text: str) -> ItemDocument:
    """Split and decode an item file the way markdown.Parse + readItemFile do."""
    lines = text.split("\n")
    if not lines or lines[0].strip() != DELIMITER:
        raise ItemParseError("missing frontmatter")
    end = next((index for index in range(1, len(lines)) if lines[index].strip() == DELIMITER), -1)
    if end < 0:
        raise ItemParseError("unterminated frontmatter")
    header = lines[1:end]
    body = "\n".join(lines[end + 1:])
    if body.startswith("\n"):
        body = body[1:]
    entries = _entries(header)
    fields: dict[str, object] = {}
    for key, indices in entries:
        if key.startswith("_"):
            continue
        if key in ITEM_LIST_KEYS:
            fields[key] = _entry_sequence(header, indices)
        else:
            fields[key] = _entry_scalar(header, indices)
    return ItemDocument(lines, end, header, body, fields, entries)


def _canonical_time(raw: object) -> str:
    """Format like Go's time.RFC3339Nano in UTC; empty when zero."""
    value = str(raw).strip()
    if not value:
        return ""
    match = _TIME_RE.match(value)
    if not match:
        raise ItemParseError(f"not an RFC 3339 time: {value!r}")
    year, month, day, hour, minute, second = (int(part) for part in match.group(1, 2, 3, 4, 5, 6))
    moment = datetime.datetime(year, month, day, hour, minute, second, tzinfo=datetime.timezone.utc)
    offset = match.group(8)
    if offset != "Z":
        sign = 1 if offset[0] == "+" else -1
        moment -= sign * datetime.timedelta(hours=int(offset[1:3]), minutes=int(offset[4:6]))
    fraction = (match.group(7) or "")[1:].rstrip("0")
    text = moment.strftime("%Y-%m-%dT%H:%M:%S")
    return text + ("." + fraction if fraction else "") + "Z"


def _collapse_space(value: object) -> str:
    return " ".join(str(value).split())


def normalize_markdown(body: str) -> str:
    """domain.NormalizeMarkdown: LF endings, no trailing spaces, no blank edges."""
    body = body.replace("\r\n", "\n").replace("\r", "\n")
    body = "\n".join(line.rstrip() for line in body.split("\n"))
    return body.strip("\n")


def _normalize_tags(values: object) -> list[str]:
    tags = []
    for value in values if isinstance(values, list) else []:
        collapsed = _collapse_space(value)
        if collapsed:
            tags.append(collapsed)
    tags.sort()
    return tags


def _write_field(buffer: bytearray, value: str) -> None:
    raw = value.encode("utf-8", "surrogateescape")
    buffer += len(raw).to_bytes(8, "big")
    buffer += raw


def item_hash(doc: ItemDocument) -> str:
    """domain.Item.ComputeHash over the parsed document."""
    fields = doc.fields
    deleted = str(fields.get("deleted", "")).strip().lower() == "true"
    buffer = bytearray()
    _write_field(buffer, "item-v1")
    _write_field(buffer, "1" if deleted else "0")
    if not deleted:
        _write_field(buffer, str(fields.get("kind", "")))
        _write_field(buffer, _collapse_space(fields.get("title", "")))
        _write_field(buffer, normalize_markdown(doc.body))
        _write_field(buffer, str(fields.get("url", "")).strip())
        _write_field(buffer, _collapse_space(fields.get("author", "")))
        _write_field(buffer, "\x00".join(_normalize_tags(fields.get("labels"))))
        _write_field(buffer, "\x00".join(_normalize_tags(fields.get("acl"))))
        _write_field(buffer, _canonical_time(fields.get("created_at", "")))
        _write_field(buffer, _canonical_time(fields.get("updated_at", "")))
        _write_field(buffer, str(fields.get("project", "")).strip())
        state = str(fields.get("state", ""))
        if state:
            _write_field(buffer, "state")
            _write_field(buffer, state)
    return ITEM_HASH_PREFIX + hashlib.sha256(bytes(buffer)).hexdigest()


def scrub_item(text: str, connection_id: str) -> tuple[str, int, int, bool]:
    """Scrub the fields the ingest policy cleans and refresh the content hash.

    Returns (new text, replacements, placeholders seen, hash rewritten).
    """
    doc = parse_item(text)

    def replace(match: re.Match[str]) -> str:
        nonlocal replaced, hashed
        token = match.group(0)
        if token.startswith("[email:"):
            hashed += 1
            return token
        replaced += 1
        return placeholder(connection_id, token.encode("ascii")).decode("ascii")

    replaced = 0
    hashed = 0
    for key, indices in doc.entries:
        if key not in ITEM_SCRUB_KEYS:
            continue
        for index in indices:
            line_index = index + 1
            doc.lines[line_index] = EMAIL_TEXT_RE.sub(replace, doc.lines[line_index])
    for index in range(doc.end + 1, len(doc.lines)):
        doc.lines[index] = EMAIL_TEXT_RE.sub(replace, doc.lines[index])
    if not replaced:
        return text, 0, hashed, False

    refreshed = parse_item(doc.text)
    digest = item_hash(refreshed)
    hash_lines = [indices for key, indices in refreshed.entries if key == "hash"]
    stored = str(refreshed.fields.get("hash", ""))
    if not hash_lines or stored == digest:
        return refreshed.text, replaced, hashed, False
    line_index = hash_lines[0][0] + 1
    line = doc.lines[line_index]
    head, _, tail = line.partition(":")
    if tail.lstrip().startswith('"'):
        lead = tail[: len(tail) - len(tail.lstrip())]
        doc.lines[line_index] = f'{head}:{lead}"{digest}"'
    else:
        lead = tail[: len(tail) - len(tail.lstrip())] or " "
        doc.lines[line_index] = f"{head}:{lead}{digest}"
    return doc.text, replaced, hashed, True


@dataclass
class GroupStat:
    files: int = 0
    changed: int = 0
    matches: int = 0


@dataclass
class Report:
    files_scanned: int = 0
    files_changed: int = 0
    matches: int = 0
    already_hashed: int = 0
    hashes_updated: int = 0
    unparsed: int = 0
    groups: dict[str, GroupStat] = field(default_factory=dict)
    examples: list[tuple[str, int]] = field(default_factory=list)
    applied: bool = False

    def group(self, name: str) -> GroupStat:
        if name not in self.groups:
            self.groups[name] = GroupStat()
        return self.groups[name]


class Connections:
    """Connections from connectors/connections.json, for connection-scoped hashes."""

    def __init__(self, records: list[dict]) -> None:
        self.records = records

    @classmethod
    def load(cls, data_dir: Path) -> "Connections":
        path = data_dir / "connectors" / "connections.json"
        try:
            records = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return cls([])
        except (OSError, json.JSONDecodeError) as exc:
            raise SystemExit(f"scrub-raw-pii: read {path}: {exc}") from exc
        if not isinstance(records, list):
            raise SystemExit(f"scrub-raw-pii: {path}: expected a JSON array")
        return cls([record for record in records if isinstance(record, dict)])

    def unique_of_provider(self, provider: str) -> str:
        ids = [str(r.get("id", "")) for r in self.records if r.get("provider") == provider]
        ids = [connection_id for connection_id in ids if connection_id]
        return ids[0] if len(ids) == 1 else provider

    def for_account(self, account: str) -> str:
        for record in self.records:
            if record.get("provider") == "imap" and record.get("account") == account:
                return str(record.get("id", "")) or f"imap-{account}"
        return f"imap-{account}"


def walk_files(base: Path):
    """Regular files below base, deterministic order, .git and symlinks skipped."""
    if not base.is_dir():
        return
    for current, dirs, files in os.walk(base):
        dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
        for name in sorted(files):
            path = Path(current) / name
            if path.is_symlink() or not path.is_file():
                continue
            yield path


def connection_for(zone: str, parts: list[str], data: bytes, connections: Connections) -> str:
    """Connection namespace of one file, so its hashes match that connection's ingests."""
    if zone == "items":
        # connectors/<connectionID>/items/<file>
        return parts[1]
    if zone == "runs":
        match = RUN_SOURCE_RE.search(data[:4096])
        if match:
            provider = match.group(1).decode("ascii", "replace")
            return connections.unique_of_provider(provider)
        match = RUN_NAME_RE.match(parts[-1])
        if match:
            return connections.unique_of_provider(match.group("provider"))
        return "runs"
    # dumps: mail/<account>/...; github/...; gitlab/...
    if parts[0] == "mail" and len(parts) >= 3:
        return connections.for_account(parts[1])
    return connections.unique_of_provider(parts[0])


def zone_files(data_dir: Path, zones: set[str]):
    """(absolute path, zone, group) for every file of the selected raw zones."""
    if "items" in zones:
        connectors = data_dir / "connectors"
        if connectors.is_dir():
            for entry in sorted(connectors.iterdir(), key=lambda p: p.name):
                if not entry.is_dir() or entry.name in SKIP_DIRS:
                    continue
                items = entry / "items"
                if not items.is_dir():
                    continue
                for path in walk_files(items):
                    yield path, "items", entry.name
    if "runs" in zones:
        for path in walk_files(data_dir / "connectors" / "runs"):
            yield path, "runs", "runs"
    if "dumps" in zones:
        for root in DUMP_ROOTS:
            for path in walk_files(data_dir / root):
                yield path, "dumps", f"dumps/{root}"


def write_bytes_preserving(path: Path, data: bytes) -> None:
    """Replace the file atomically, keeping its mode and every non-matched byte."""
    tmp = path.with_name(path.name + ".scrub-tmp")
    try:
        with open(tmp, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        shutil.copymode(path, tmp)
        os.replace(tmp, path)
    finally:
        if tmp.exists():
            tmp.unlink()


def scrub_tree(data_dir: Path, zones: set[str], apply: bool) -> Report:
    connections = Connections.load(data_dir)
    report = Report(applied=apply)
    for path, zone, group in zone_files(data_dir, zones):
        rel = path.relative_to(data_dir).as_posix()
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise SystemExit(f"scrub-raw-pii: read {path}: {exc}") from exc
        connection_id = connection_for(zone, rel.split("/"), data, connections)
        stat = report.group(group)
        stat.files += 1
        report.files_scanned += 1

        scrubbed = data
        replaced = hashed = 0
        hash_updated = False
        if zone == "items" and path.suffix.lower() == ".md":
            try:
                text, replaced, hashed, hash_updated = scrub_item(
                    data.decode("utf-8", "surrogateescape"), connection_id
                )
            except ItemParseError as exc:
                report.unparsed += 1
                print(f"scrub-raw-pii: skip {rel}: {exc}", file=sys.stderr)
                continue
            scrubbed = text.encode("utf-8", "surrogateescape")
        else:
            scrubbed, replaced, hashed = scrub_bytes(data, connection_id)

        report.already_hashed += hashed
        if not replaced:
            continue
        report.matches += replaced
        stat.matches += replaced
        if len(report.examples) < 3:
            report.examples.append((rel, replaced))
        if scrubbed == data:
            continue
        stat.changed += 1
        report.files_changed += 1
        report.hashes_updated += 1 if hash_updated else 0
        if apply:
            try:
                write_bytes_preserving(path, scrubbed)
            except OSError as exc:
                raise SystemExit(f"scrub-raw-pii: write {path}: {exc}") from exc
    return report


def redact_path(text: str) -> str:
    return PATH_EMAIL_RE.sub("[email:redacted]", text)


def print_report(data_dir: Path, zones: set[str], report: Report, out) -> None:
    mode = "apply" if report.applied else "dry run (nothing written; pass --apply)"
    print(f"scrub-raw-pii: {mode}", file=out)
    print(f"data:  {data_dir}", file=out)
    print(f"zones: {','.join(sorted(zones))}", file=out)
    name_width = max([len("directory"), *(len(name) for name in report.groups)])
    print(f"{'directory'.ljust(name_width)}  {'files':>7}  {'changed':>7}  {'matches':>7}", file=out)
    for name in sorted(report.groups):
        stat = report.groups[name]
        print(
            f"{name.ljust(name_width)}  {stat.files:>7}  {stat.changed:>7}  {stat.matches:>7}",
            file=out,
        )
    if report.examples:
        print("examples (values redacted):", file=out)
        for display, count in report.examples:
            print(f"  {redact_path(display)}: {count} match(es)", file=out)
    print(f"files_scanned={report.files_scanned}", file=out)
    print(f"files_changed={report.files_changed}", file=out)
    print(f"matches={report.matches}", file=out)
    print(f"already_hashed={report.already_hashed}", file=out)
    print(f"hashes_updated={report.hashes_updated}", file=out)
    print(f"unparsed={report.unparsed}", file=out)


def self_test() -> int:
    failures: list[str] = []

    def check(condition: bool, message: str) -> None:
        if not condition:
            failures.append(message)

    with tempfile.TemporaryDirectory(prefix="scrub-pii-selftest-") as tmp:
        root = Path(tmp)
        items = root / "connectors" / "work" / "items"
        items.mkdir(parents=True)
        item_path = items / ("0" * 40 + ".md")
        item_path.write_bytes(FIXTURE.read_bytes())

        # A legacy dump proves the byte-exact path: CRLF kept, no trailing
        # newline added, every non-matched byte untouched.
        dump_dir = root / "mail" / "work" / "inbox"
        dump_dir.mkdir(parents=True)
        dump_path = dump_dir / "2024-01-02-uid1-2.md"
        dump_raw = b"To: Alice@Example.COM\r\nsecond alice@example.com\nno newline at end"
        dump_path.write_bytes(dump_raw)
        dump_placeholder = placeholder("imap-work", b"alice@example.com")
        want_dump = dump_raw.replace(b"Alice@Example.COM", dump_placeholder).replace(
            b"alice@example.com", dump_placeholder
        )

        already_path = root / "mail" / "work" / "sent" / "x.md"
        already_path.parent.mkdir(parents=True)
        already_raw = b'author: "[email:0123456789ab]"\r\nalready scrubbed\n'
        already_path.write_bytes(already_raw)

        before = parse_item(item_path.read_text(encoding="utf-8"))
        check(item_hash(before) == before.fields.get("hash"),
              "fixture: stored hash does not match its content before the scrub")

        dry = scrub_tree(root, {"items", "dumps"}, apply=False)
        check(dry.files_scanned == 3, f"dry: files_scanned={dry.files_scanned}, want 3")
        check(dry.files_changed == 2, f"dry: files_changed={dry.files_changed}, want 2")
        check(dry.matches == 8, f"dry: matches={dry.matches}, want 8")
        check(dry.hashes_updated == 1, f"dry: hashes_updated={dry.hashes_updated}, want 1")
        check(dump_path.read_bytes() == dump_raw, "dry: dump was rewritten")
        check(item_path.read_bytes() == FIXTURE.read_bytes(), "dry: item was rewritten")

        first = scrub_tree(root, {"items", "dumps"}, apply=True)
        check(first.files_changed == 2, f"apply: files_changed={first.files_changed}, want 2")
        check(first.matches == 8, f"apply: matches={first.matches}, want 8")
        check(first.hashes_updated == 1, f"apply: hashes_updated={first.hashes_updated}, want 1")
        check(first.unparsed == 0, f"apply: unparsed={first.unparsed}, want 0")

        got_dump = dump_path.read_bytes()
        check(got_dump == want_dump, f"apply: dump={got_dump!r}, want {want_dump!r}")
        check(b"\r\n" in got_dump, "apply: CRLF line ending lost")
        check(got_dump.endswith(b"no newline at end"), "apply: trailing bytes changed")
        check(re.fullmatch(rb"\[email:[0-9a-f]{12}\]", dump_placeholder) is not None,
              f"placeholder format: {dump_placeholder!r}")
        check(already_path.read_bytes() == already_raw, "apply: already hashed file changed")

        # The scrubbed item must carry exactly the hash the engine writes for
        # that content (go run scripts/scrub-raw-pii-hash-check.go -print-hash).
        after = parse_item(item_path.read_text(encoding="utf-8"))
        check(item_hash(after) == after.fields.get("hash"),
              "fixture: stored hash is stale after the scrub")
        check(after.fields.get("hash") == FIXTURE_SCRUBBED_HASH,
              f"fixture: hash={after.fields.get('hash')}, want {FIXTURE_SCRUBBED_HASH}")
        check(b"@example.com" not in item_path.read_bytes(), "fixture: raw address left")

        second = scrub_tree(root, {"items", "dumps"}, apply=True)
        check(second.files_changed == 0, f"second apply: files_changed={second.files_changed}, want 0")
        check(second.matches == 0, f"second apply: matches={second.matches}, want 0")
        check(second.already_hashed == 9, f"second apply: already_hashed={second.already_hashed}, want 9")
        check(second.hashes_updated == 0, f"second apply: hashes_updated={second.hashes_updated}, want 0")
        check(item_path.read_bytes()
              == parse_item(item_path.read_text(encoding="utf-8")).text.encode("utf-8"),
              "second apply: item bytes changed")

    if failures:
        for failure in failures:
            print(f"self-test FAIL: {failure}", file=sys.stderr)
        return 1
    print("self-test OK: byte-exact scrub, engine hash refreshed, idempotent")
    return 0


def parse_include(value: str) -> set[str]:
    zones = {part.strip().lower() for part in value.split(",") if part.strip()}
    unknown = zones - set(ZONES)
    if unknown:
        raise argparse.ArgumentTypeError(
            f"unknown zone(s) {', '.join(sorted(unknown))}; valid: {','.join(ZONES)}"
        )
    if not zones:
        raise argparse.ArgumentTypeError("--include needs at least one zone")
    return zones


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Scrub e-mail addresses in lv24-brain raw zones with the ingest placeholder."
    )
    parser.add_argument("data_dir", nargs="?", help="data repository root (dry run by default)")
    parser.add_argument("--apply", action="store_true", help="rewrite matched files")
    parser.add_argument(
        "--include",
        type=parse_include,
        default=set(ZONES),
        help=f"comma-separated zones (default: {','.join(ZONES)})",
    )
    parser.add_argument("--self-test", action="store_true", help="run the built-in fixture test and exit")
    args = parser.parse_args(argv)

    if args.self_test:
        return self_test()
    if not args.data_dir:
        parser.error("a data directory is required unless --self-test is used")
    data_dir = Path(args.data_dir).expanduser().resolve()
    if not data_dir.is_dir():
        parser.error(f"not a directory: {data_dir}")
    if args.apply and (data_dir / ".git").exists():
        print(
            f"scrub-raw-pii: warning: {data_dir} is a git checkout; --apply rewrites tracked files",
            file=sys.stderr,
        )
    report = scrub_tree(data_dir, args.include, args.apply)
    print_report(data_dir, args.include, report, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
