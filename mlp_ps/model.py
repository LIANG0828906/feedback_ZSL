from __future__ import annotations

from typing import Dict, Iterable, List, Mapping, Sequence, Tuple

import torch
from torch import nn


class TwoLayerMLP(nn.Module):
    def __init__(self, input_dim: int, hidden_dim: int, output_dim: int) -> None:
        super().__init__()
        self.fc1 = nn.Linear(input_dim, hidden_dim)
        self.activation = nn.ReLU()
        self.fc2 = nn.Linear(hidden_dim, output_dim)

    def forward(self, inputs: torch.Tensor) -> torch.Tensor:
        return self.fc2(self.activation(self.fc1(inputs)))


def build_model(seed: int, input_dim: int, hidden_dim: int, output_dim: int) -> TwoLayerMLP:
    with torch.random.fork_rng(devices=[]):
        torch.manual_seed(seed)
        model = TwoLayerMLP(input_dim, hidden_dim, output_dim)
    model = model.to(device="cpu", dtype=torch.float32)
    model.train()
    return model


def ordered_parameter_names(model: nn.Module) -> List[str]:
    return [name for name, _ in model.named_parameters()]


def parameter_shapes(model: nn.Module) -> List[Tuple[str, Tuple[int, ...]]]:
    return [(name, tuple(param.shape)) for name, param in model.named_parameters()]


def model_state_by_name(model: nn.Module) -> Dict[str, torch.Tensor]:
    return {name: param.detach().cpu().contiguous() for name, param in model.named_parameters()}


def load_model_state(model: nn.Module, state: Mapping[str, torch.Tensor]) -> None:
    with torch.no_grad():
        for name, param in model.named_parameters():
            if name not in state:
                raise KeyError(name)
            value = state[name]
            if tuple(value.shape) != tuple(param.shape):
                raise ValueError(
                    f"parameter shape mismatch for {name}: {tuple(value.shape)} != {tuple(param.shape)}"
                )
            param.copy_(value.to(device=param.device, dtype=param.dtype))


def zero_gradients(model: nn.Module) -> None:
    for param in model.parameters():
        param.grad = None


def full_gradient_snapshot(model: nn.Module) -> Dict[str, torch.Tensor]:
    result: Dict[str, torch.Tensor] = {}
    for name, param in model.named_parameters():
        if param.grad is None:
            result[name] = torch.zeros_like(param.detach(), dtype=torch.float32)
        else:
            result[name] = param.grad.detach().cpu().contiguous().clone()
    return result


def average_gradients(
    gradients: Sequence[Mapping[str, torch.Tensor]],
    parameter_names: Sequence[str],
) -> Dict[str, torch.Tensor]:
    if not gradients:
        raise ValueError("at least one gradient source is required")
    result: Dict[str, torch.Tensor] = {}
    for name in parameter_names:
        total = gradients[0][name].to(dtype=torch.float32).clone()
        for source in gradients[1:]:
            total.add_(source[name].to(dtype=torch.float32))
        total.div_(float(len(gradients)))
        result[name] = total
    return result


def assign_gradients(model: nn.Module, gradients: Mapping[str, torch.Tensor]) -> None:
    for name, param in model.named_parameters():
        param.grad = gradients[name].to(device=param.device, dtype=param.dtype).clone()


def gradient_norm(gradients: Mapping[str, torch.Tensor]) -> float:
    total = 0.0
    for name in sorted(gradients):
        value = torch.linalg.vector_norm(gradients[name].to(dtype=torch.float64))
        total += float(value.item()) ** 2
    return total ** 0.5


def evaluate_loss(
    model: nn.Module,
    features: torch.Tensor,
    targets: torch.Tensor,
) -> float:
    model.eval()
    with torch.no_grad():
        prediction = model(features)
        loss = torch.mean((prediction - targets) ** 2)
    model.train()
    return float(loss.item())


def build_optimizer(model: nn.Module, learning_rate: float, momentum: float) -> torch.optim.SGD:
    return torch.optim.SGD(
        model.parameters(),
        lr=learning_rate,
        momentum=momentum,
        dampening=0.0,
        weight_decay=0.0,
        nesterov=False,
        foreach=False,
    )
