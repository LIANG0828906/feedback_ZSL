from __future__ import annotations

from dataclasses import dataclass


@dataclass(slots=True)
class Token:
    kind: str
    text: str
    start: int
    end: int
    depth: int

    @property
    def lower(self) -> str:
        return self.text.lower()


def tokenize(sql: str) -> list[Token]:
    tokens: list[Token] = []
    i = 0
    depth = 0
    n = len(sql)
    while i < n:
        char = sql[i]
        if char.isspace():
            i += 1
            continue
        if sql.startswith("--", i):
            end = sql.find("\n", i + 2)
            if end < 0:
                end = n
            tokens.append(Token("comment", sql[i:end], i, end, depth))
            i = end
            continue
        if sql.startswith("/*", i):
            start = i
            i += 2
            nesting = 1
            while i < n and nesting:
                if sql.startswith("/*", i):
                    nesting += 1
                    i += 2
                elif sql.startswith("*/", i):
                    nesting -= 1
                    i += 2
                else:
                    i += 1
            tokens.append(Token("comment", sql[start:i], start, i, depth))
            continue
        if char == "'" or (
            char in "Nn" and i + 1 < n and sql[i + 1] == "'" and (i == 0 or not _ident_char(sql[i - 1]))
        ):
            start = i
            if char in "Nn":
                i += 1
            i += 1
            while i < n:
                if sql[i] == "'":
                    if i + 1 < n and sql[i + 1] == "'":
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            tokens.append(Token("string", sql[start:i], start, i, depth))
            continue
        if char == "[":
            start = i
            i += 1
            while i < n:
                if sql[i] == "]":
                    if i + 1 < n and sql[i + 1] == "]":
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            tokens.append(Token("bracket", sql[start:i], start, i, depth))
            continue
        if char == '"':
            start = i
            i += 1
            while i < n:
                if sql[i] == '"':
                    if i + 1 < n and sql[i + 1] == '"':
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            tokens.append(Token("quoted", sql[start:i], start, i, depth))
            continue
        if char.isdigit():
            start = i
            i += 1
            while i < n and (sql[i].isdigit() or sql[i] in ".eE+-" and _number_continuation(sql, i)):
                i += 1
            tokens.append(Token("number", sql[start:i], start, i, depth))
            continue
        if _ident_start(char):
            start = i
            i += 1
            while i < n and _ident_char(sql[i]):
                i += 1
            tokens.append(Token("word", sql[start:i], start, i, depth))
            continue
        if char == "(":
            tokens.append(Token("punct", char, i, i + 1, depth))
            depth += 1
            i += 1
            continue
        if char == ")":
            depth = max(0, depth - 1)
            tokens.append(Token("punct", char, i, i + 1, depth))
            i += 1
            continue
        two = sql[i : i + 2]
        if two in {"<=", ">=", "<>", "!=", "+=", "-=", "*=", "/=", "||"}:
            tokens.append(Token("punct", two, i, i + 2, depth))
            i += 2
            continue
        tokens.append(Token("punct", char, i, i + 1, depth))
        i += 1
    return tokens


def _ident_start(char: str) -> bool:
    return char.isalpha() or char in "_@#"


def _ident_char(char: str) -> bool:
    return char.isalnum() or char in "_@#$"


def _number_continuation(sql: str, index: int) -> bool:
    if sql[index] not in "+-":
        return True
    if index == 0:
        return False
    return sql[index - 1] in "eE"


def mask_tokens(sql: str, tokens: list[Token], kinds: set[str]) -> str:
    chars = list(sql)
    for token in tokens:
        if token.kind not in kinds:
            continue
        for index in range(token.start, token.end):
            if chars[index] not in "\r\n":
                chars[index] = " "
    return "".join(chars)


def mask_comments_and_strings(sql: str) -> str:
    return mask_tokens(sql, tokenize(sql), {"comment", "string"})


def mask_comments(sql: str) -> str:
    return mask_tokens(sql, tokenize(sql), {"comment"})


def decode_string(token: Token) -> str:
    text = token.text
    if text[:1] in "Nn":
        text = text[1:]
    if len(text) >= 2 and text[0] == "'" and text[-1] == "'":
        text = text[1:-1]
    return text.replace("''", "'")


def identifier(token: Token) -> str:
    if token.kind == "bracket":
        return token.text[1:-1].replace("]]", "]")
    if token.kind == "quoted":
        return token.text[1:-1].replace('""', '"')
    return token.text


def replace_spans(sql: str, replacements: list[tuple[int, int, str]]) -> str:
    if not replacements:
        return sql
    ordered = sorted(replacements, key=lambda item: item[0], reverse=True)
    output = sql
    last_start = len(sql) + 1
    for start, end, text in ordered:
        if end > last_start:
            continue
        output = output[:start] + text + output[end:]
        last_start = start
    return output
