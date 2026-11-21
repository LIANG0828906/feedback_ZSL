"""A small scalar reverse-mode automatic differentiation engine.

Each image pixel is evaluated as an independent graph. Image chunks can
therefore be computed in any worker without sharing mutable graph state.
"""

from __future__ import annotations

import math
from collections.abc import Callable


class Var:
    __slots__ = ("value", "grad", "_parents", "_requires_grad")

    def __init__(
        self,
        value: float,
        requires_grad: bool = False,
        parents: tuple[tuple["Var", Callable[[float], None]], ...] = (),
    ) -> None:
        self.value = float(value)
        self.grad = 0.0
        self._parents = parents
        self._requires_grad = requires_grad or bool(parents)

    @classmethod
    def constant(cls, value: float) -> "Var":
        return cls(value)

    @classmethod
    def root(cls, value: float) -> "Var":
        return cls(value, requires_grad=True)

    def backward(self) -> None:
        self.grad = 1.0
        order: list[Var] = []
        visited: set[int] = set()
        stack: list[tuple[Var, bool]] = [(self, False)]
        while stack:
            node, expanded = stack.pop()
            if expanded:
                order.append(node)
                continue
            node_id = id(node)
            if node_id in visited:
                continue
            visited.add(node_id)
            stack.append((node, True))
            for parent, _ in node._parents:
                if id(parent) not in visited:
                    stack.append((parent, False))
        for node in reversed(order):
            if node.grad == 0.0:
                continue
            for parent, propagate in node._parents:
                if parent._requires_grad:
                    propagate(node.grad)

    def __add__(self, other: object) -> "Var":
        other_var = _as_var(other)
        result = Var(self.value + other_var.value)

        def left(upstream: float) -> None:
            self.grad += upstream

        def right(upstream: float) -> None:
            other_var.grad += upstream

        result._parents = ((self, left), (other_var, right))
        result._requires_grad = self._requires_grad or other_var._requires_grad
        return result

    def __radd__(self, other: object) -> "Var":
        return _as_var(other).__add__(self)

    def __sub__(self, other: object) -> "Var":
        other_var = _as_var(other)
        result = Var(self.value - other_var.value)

        def left(upstream: float) -> None:
            self.grad += upstream

        def right(upstream: float) -> None:
            other_var.grad -= upstream

        result._parents = ((self, left), (other_var, right))
        result._requires_grad = self._requires_grad or other_var._requires_grad
        return result

    def __rsub__(self, other: object) -> "Var":
        return _as_var(other).__sub__(self)

    def __mul__(self, other: object) -> "Var":
        other_var = _as_var(other)
        result = Var(self.value * other_var.value)
        left_value = other_var.value
        right_value = self.value

        def left(upstream: float) -> None:
            self.grad += upstream * left_value

        def right(upstream: float) -> None:
            other_var.grad += upstream * right_value

        result._parents = ((self, left), (other_var, right))
        result._requires_grad = self._requires_grad or other_var._requires_grad
        return result

    def __rmul__(self, other: object) -> "Var":
        return _as_var(other).__mul__(self)

    def __truediv__(self, other: object) -> "Var":
        other_var = _as_var(other)
        denominator = other_var.value
        if denominator == 0.0:
            raise ZeroDivisionError("autodiff division by zero")
        result = Var(self.value / denominator)
        numerator_value = self.value

        def left(upstream: float) -> None:
            self.grad += upstream / denominator

        def right(upstream: float) -> None:
            other_var.grad -= upstream * numerator_value / (denominator * denominator)

        result._parents = ((self, left), (other_var, right))
        result._requires_grad = self._requires_grad or other_var._requires_grad
        return result

    def __rtruediv__(self, other: object) -> "Var":
        return _as_var(other).__truediv__(self)

    def __neg__(self) -> "Var":
        return self * -1.0

    def __pow__(self, exponent: float) -> "Var":
        value = self.value ** exponent
        result = Var(value)
        derivative = exponent * (self.value ** (exponent - 1.0))

        def parent(upstream: float) -> None:
            self.grad += upstream * derivative

        result._parents = ((self, parent),)
        result._requires_grad = self._requires_grad
        return result


def _as_var(value: object) -> Var:
    if isinstance(value, Var):
        return value
    if isinstance(value, (int, float)):
        return Var(float(value))
    raise TypeError(f"unsupported autodiff operand: {type(value)!r}")


def sqrt(value: Var) -> Var:
    if value.value < 0.0:
        raise ValueError("sqrt of negative autodiff value")
    root = math.sqrt(value.value)
    result = Var(root)
    derivative = 0.0 if root == 0.0 else 0.5 / root

    def parent(upstream: float) -> None:
        value.grad += upstream * derivative

    result._parents = ((value, parent),)
    result._requires_grad = value._requires_grad
    return result


def sin(value: Var) -> Var:
    result = Var(math.sin(value.value))
    derivative = math.cos(value.value)

    def parent(upstream: float) -> None:
        value.grad += upstream * derivative

    result._parents = ((value, parent),)
    result._requires_grad = value._requires_grad
    return result


def cos(value: Var) -> Var:
    result = Var(math.cos(value.value))
    derivative = -math.sin(value.value)

    def parent(upstream: float) -> None:
        value.grad += upstream * derivative

    result._parents = ((value, parent),)
    result._requires_grad = value._requires_grad
    return result


def maximum(value: Var, floor: float) -> Var:
    if value.value > floor:
        result = Var(value.value)

        def parent(upstream: float) -> None:
            value.grad += upstream

        result._parents = ((value, parent),)
        result._requires_grad = value._requires_grad
        return result
    return Var(floor)


def norm3(vector: tuple[Var, Var, Var]) -> Var:
    squared = vector[0] * vector[0] + vector[1] * vector[1] + vector[2] * vector[2]
    return sqrt(squared)


def dot3(left: tuple[Var, Var, Var], right: tuple[Var, Var, Var]) -> Var:
    return left[0] * right[0] + left[1] * right[1] + left[2] * right[2]

