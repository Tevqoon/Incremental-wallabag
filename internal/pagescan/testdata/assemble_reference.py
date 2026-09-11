# Kept here (not deleted after the Go port landed) purely as documentation:
# this is the script that produced ../expected.json, so re-running it is how
# you regenerate that file if the golden scans ever change.
"""Reference implementation of page-scan assembly (the Go port must match).

Usage: python3 assemble.py SCAN_JSON... > expected.json
Each SCAN_JSON is one photo's model response ({"pages": [...]}), scan ids are
1, 2, 3... in argument order.
"""
import json, re, sys

PARA = "¶"
TERMINAL = r'[.?!…]["”’)\]]*'
END_OF_LINE = re.compile(TERMINAL + r'$')
END = re.compile(TERMINAL + r'(?=\s|$)')
START = re.compile(TERMINAL + r'\s+(?=["“‘(\[]?[A-Z])')
SMALL = {"a", "an", "and", "as", "at", "but", "by", "for", "from", "in", "nor",
         "of", "on", "or", "the", "to", "via", "vs", "with"}
ROMAN = {"i": 1, "v": 5, "x": 10, "l": 50, "c": 100, "d": 500, "m": 1000}
CHAPTER_HEAD = re.compile(r'^chapter\s+(\S+)$', re.I)


def roman(s):
    s = s.lower()
    if not s or any(c not in ROMAN for c in s):
        return None
    total = 0
    for i, c in enumerate(s):
        v = ROMAN[c]
        total += -v if i + 1 < len(s) and ROMAN[s[i + 1]] > v else v
    return total


def page_number(label):
    """(key, number) — key orders front matter (roman) before the body (arabic)."""
    if label is None:
        return None, None
    label = label.strip()
    if label.isdigit():
        n = int(label)
        return 10000 + n, n
    r = roman(label)
    if r is not None:
        return r, r
    return None, None


def title_case(head):
    words = head.split()
    if head.upper() != head:
        return " ".join(words)  # already mixed case: keep as printed
    out = []
    for i, w in enumerate(words):
        lw = w.lower()
        out.append(lw if 0 < i < len(words) - 1 and lw in SMALL else lw[:1].upper() + lw[1:])
    return " ".join(out)


def ends_sentence(line):
    return bool(END_OF_LINE.search(line.strip()))


def word_before_hyphen(line):
    m = re.search(r'(\w+)-$', line)
    return m.group(1) if m else ""


def word_after(line):
    m = re.match(r'(\w+)', line)
    return m.group(1) if m else ""


def glue(left, right, model_text):
    """Join two consecutive lines; a line-final hyphen is kept only when the
    model's own passage text spells the word with it (self-interest)."""
    if left.endswith("-") and right[:1].islower():
        compound = word_before_hyphen(left) + "-" + word_after(right)
        if compound != "-" and compound in " ".join((model_text or "").split()):
            return left + right
        return left[:-1] + right
    return left + " " + right


def fragment(page, mark, index):
    lines = [l.strip() for l in page["lines"]]
    paras = [l.startswith(PARA) for l in lines]
    lines = [l.lstrip(PARA).strip() for l in lines]
    n = len(lines)
    f = max(1, min(int(mark["first_line"]), n)) - 1
    l = max(1, min(int(mark["last_line"]), n)) - 1
    if l < f:
        f, l = l, f
    text = lines[f]
    for i in range(f + 1, l + 1):
        text = text + "\n\n" + lines[i] if paras[i] else glue(text, lines[i], mark.get("text"))
    starts_clean = paras[f] or (f > 0 and ends_sentence(lines[f - 1])) or (f == 0 and lines[0][:1].isupper())
    return {
        "text": text,
        "starts_mid": not starts_clean,
        "ends_mid": not ends_sentence(lines[l]),
        "at_top": f == 0,
        "at_bottom": l == n - 1,
        "has_handwriting": bool(mark.get("has_handwriting")),
        "model_text": mark.get("text") or "",
        "index": index,
    }


def trim_start(frag_text):
    """Drop everything before the first sentence start; unchanged if none."""
    hit = START.search(frag_text)
    return frag_text[hit.end():] if hit else frag_text


def trim_end(frag_text):
    """Drop everything after the last sentence end; unchanged if none."""
    ends = [e.end() for e in END.finditer(frag_text)]
    return frag_text[:ends[-1]] if ends else frag_text


def load(paths):
    pages = []
    for scan_id, path in enumerate(paths, start=1):
        for page_index, page in enumerate(json.load(open(path))["pages"]):
            key, number = page_number(page.get("page_label"))
            pages.append({"scan_id": scan_id, "page_index": page_index, "key": key, "number": number,
                          "label": (page.get("page_label") or "").strip(), "page": page})
    # Unlabelled pages sit just after the labelled page that precedes them in
    # upload order (scan id, then position in the photo).
    last_key = 0
    for p in pages:
        if p["key"] is None:
            p["key"], p["half"] = last_key, True
        else:
            last_key, p["half"] = p["key"], False
    # Retakes: the same labelled page photographed twice — the later scan wins.
    by_label = {}
    for p in pages:
        if p["label"] and not p["half"]:
            by_label[p["label"]] = p
    pages = [p for p in pages if p["half"] or by_label.get(p["label"]) is p]
    pages.sort(key=lambda p: (p["key"], p["half"], p["scan_id"], p["page_index"]))
    return pages


def ref_prefix(p):
    return f"scan:p{p['label']}" if not p["half"] else f"scan:s{p['scan_id']}.{p['page_index']}"


def chapter_groups(pages):
    """Assign each page a group id; see the Go doc comment for the rule."""
    group, even, odd = 0, None, None
    started = False
    for p in pages:
        head = " ".join((p["page"].get("running_head") or "").split())
        if p["number"] is None or not head:
            p["group"] = group
            continue
        if p["number"] % 2 == 0:
            if even is not None and head != even:
                group, odd, started = group + 1, None, True
            even = head
        else:
            if odd is not None and head != odd:
                group, even, started = group + 1, None, True
            odd = head
        p["group"] = group
        p["even_head"], p["odd_head"] = even, odd
    names = {}
    for p in pages:
        g = p["group"]
        e, o = p.get("even_head"), p.get("odd_head")
        names.setdefault(g, [None, None])
        if e:
            names[g][0] = e
        if o:
            names[g][1] = o
    out = {}
    for g, (e, o) in names.items():
        m = CHAPTER_HEAD.match(e or "")
        if m and o:
            out[g] = f"Chapter {m.group(1)}: {title_case(o)}"
        elif o:
            out[g] = title_case(o)
        elif e:
            out[g] = title_case(e)
        else:
            out[g] = ""
    return out


def assemble(paths, existing=None):
    existing = existing or {}
    pages = load(paths)
    names = chapter_groups(pages)
    frags = []  # (page, fragment)
    for p in pages:
        for i, mark in enumerate(p["page"].get("marks") or []):
            if not isinstance(mark.get("first_line"), int) or not p["page"].get("lines"):
                continue
            frags.append((p, fragment(p["page"], mark, i)))

    passages, consumed = [], set()
    for k, (p, fr) in enumerate(frags):
        if k in consumed:
            continue
        parts = [(p, fr)]
        # Join forward while this piece runs off the bottom mid-sentence and
        # the very next page's first mark starts at its top mid-sentence.
        while True:
            lp, lf = parts[-1]
            if not (lf["at_bottom"] and lf["ends_mid"]) or k + len(parts) >= len(frags):
                break
            np_, nf = frags[k + len(parts)]
            adjacent = (lp["number"] is not None and np_["number"] == lp["number"] + 1
                        and not np_["half"] and nf["index"] == 0)
            if not (adjacent and nf["at_top"] and nf["starts_mid"]):
                break
            parts.append((np_, nf))
        for j in range(1, len(parts)):
            consumed.add(k + j)

        first_p, first_f = parts[0]
        last_p, last_f = parts[-1]
        text = first_f["text"]
        for _, nf in parts[1:]:
            text = glue(text, nf["text"], first_f["model_text"] + " " + nf["model_text"])
        if first_f["starts_mid"]:
            text = trim_start(text)
        if last_f["ends_mid"]:
            text = trim_end(text)

        ref = f"{ref_prefix(first_p)}:m{first_f['index']}"
        label = first_p["label"] if first_p is last_p else f"{first_p['label']}–{last_p['label']}"
        passages.append({
            "ref": ref,
            "absorbed_refs": [f"{ref_prefix(pp)}:m{ff['index']}" for pp, ff in parts[1:]],
            "page": label,
            "ordinal": first_p["key"] * 1000 + (500 if first_p["half"] else 0) + first_f["index"],
            "text": text.strip(),
            "note_pending": any(ff["has_handwriting"] for _, ff in parts),
            "group": first_p["group"],
            "scan_ids": sorted({pp["scan_id"] for pp, _ in parts}),
        })

    # Chapter: a group inherits the current chapter of any already-stored
    # passage in it (lowest ordinal wins), else takes the derived name.
    chapter_of_group = {}
    for ps in sorted(passages, key=lambda x: x["ordinal"]):
        if ps["ref"] in existing and existing[ps["ref"]] and ps["group"] not in chapter_of_group:
            chapter_of_group[ps["group"]] = existing[ps["ref"]]
    for ps in passages:
        ps["chapter"] = chapter_of_group.get(ps["group"], names.get(ps["group"], ""))
        del ps["group"]
    return passages


if __name__ == "__main__":
    print(json.dumps(assemble(sys.argv[1:]), indent=2, ensure_ascii=False))
