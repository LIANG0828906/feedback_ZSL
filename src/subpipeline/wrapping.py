"""Deterministic min-cost wrapping and multi-screen splitting."""

from .errors import E_PARA, PipelineError
from .model import Screen
from .samples import NO_LINE_START_PUNCT


def tokenize(text):
    """Split text into atoms.

    Each CJK character or punctuation is its own atom; consecutive ASCII
    letters/digits form one word atom. Whitespace is dropped (the generated
    corpus is space-free) but tracked as allowed break opportunities.
    """
    atoms = []
    ascii_word = []
    for ch in text:
        if ch.isspace():
            if ascii_word:
                atoms.append("".join(ascii_word))
                ascii_word = []
            continue
        if ch.isascii() and (ch.isalnum()):
            ascii_word.append(ch)
            continue
        if ascii_word:
            atoms.append("".join(ascii_word))
            ascii_word = []
        atoms.append(ch)
    if ascii_word:
        atoms.append("".join(ascii_word))
    return tuple(atoms)


def _line_cost(line_atoms, max_chars, params):
    used = sum(len(atom) for atom in line_atoms)
    ragged = (max_chars - used) ** 2
    return params.wrap_weight_raggedness * ragged


def wrap_text(text, params):
    """Return tuple of lines (each a string) using deterministic min-cost DP."""
    max_chars = params.max_chars_per_line
    atoms = tokenize(text)
    if not atoms:
        raise PipelineError(E_PARA, "empty text cannot be wrapped")
    for atom in atoms:
        if len(atom) > max_chars:
            raise PipelineError(
                E_PARA,
                f"atom of width {len(atom)} exceeds max_chars_per_line {max_chars}")

    n = len(atoms)
    # dp[i] = (min_cost, line_start_index) for wrapping atoms[i:].
    dp = [None] * (n + 1)
    dp[n] = (0.0, n)
    for i in range(n - 1, -1, -1):
        best = None
        width = 0
        for j in range(i + 1, n + 1):
            atom = atoms[j - 1]
            width += len(atom)
            if width > max_chars:
                break
            if j < n and atoms[j] in NO_LINE_START_PUNCT:
                continue  # next line would start with orphan punctuation
            line_atoms = atoms[i:j]
            cost = _line_cost(line_atoms, max_chars, params)
            candidate = (cost + dp[j][0], j)
            if best is None or candidate[0] < best[0] or \
                    (candidate[0] == best[0] and candidate[1] > best[1]):
                best = candidate
        if best is None:
            raise PipelineError(E_PARA,
                                "no legal break keeps punctuation off line start")
        dp[i] = best

    lines = []
    i = 0
    while i < n:
        j = dp[i][1]
        lines.append("".join(atoms[i:j]))
        i = j
    result = tuple(lines)
    _assert_preserves_text(text, result)
    return result


def split_screens(lines, params):
    """Greedy balanced packing of wrapped lines into screens.

    A screen never exceeds ``max_lines_per_screen`` and never starts with a
    line whose first character is a no-line-start punctuation mark; in that
    case the offending line is pulled back onto the previous screen.
    """
    cap = params.max_lines_per_screen
    screens = []
    current = []
    for line in lines:
        if current and len(current) >= cap:
            if line and line[0] in NO_LINE_START_PUNCT:
                # Pull the punctuation line back: swap last line forward.
                moved = current.pop()
                screens.append(Screen(tuple(current)))
                current = [moved]
            else:
                screens.append(Screen(tuple(current)))
                current = []
        current.append(line)
    if current:
        screens.append(Screen(tuple(current)))
    return tuple(screens)


def _assert_preserves_text(original, lines):
    rebuilt = "".join(lines)
    if rebuilt != original:
        raise PipelineError(E_PARA,
                            f"wrapping lost/duplicated chars: {rebuilt!r} != {original!r}")


def verify_screens_preserve_text(original, screens):
    rebuilt = "".join("".join(screen.lines) for screen in screens)
    return rebuilt == original


def layout_text(text, params):
    """Full layout: wrap then split into screens, with conservation checks."""
    lines = wrap_text(text, params)
    screens = split_screens(lines, params)
    if not verify_screens_preserve_text(text, screens):
        raise PipelineError(E_PARA, "screen split changed the character stream")
    for screen in screens:
        for line in screen.lines:
            if len(line) > params.max_chars_per_line:
                raise PipelineError(E_PARA, "line exceeds configured width")
    return screens


def reflow_with_screens(text, params, extra_screens):
    """Force at least ``1 + extra_screens`` screens by balanced repartition."""
    lines = wrap_text(text, params)
    target = min(len(lines), 1 + extra_screens)
    if target <= 1 or len(lines) == 1:
        return split_screens(lines, params)
    base = max(1, len(lines) // target)
    screens = []
    chunk = []
    for line in lines:
        if len(chunk) >= base and len(screens) < target - 1:
            if line and line[0] in NO_LINE_START_PUNCT and chunk:
                moved = chunk.pop()
                screens.append(Screen(tuple(chunk)))
                chunk = [moved]
            else:
                screens.append(Screen(tuple(chunk)))
                chunk = []
        chunk.append(line)
    if chunk:
        screens.append(Screen(tuple(chunk)))
    if not verify_screens_preserve_text(text, screens):
        raise PipelineError(E_PARA, "forced reflow changed the character stream")
    return tuple(screens)
