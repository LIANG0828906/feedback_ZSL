from __future__ import annotations

import warnings

warnings.filterwarnings(
    "ignore",
    message=r"Failed to initialize NumPy: No module named 'numpy'.*",
    category=UserWarning,
)

import torch
from torch import nn


def main() -> int:
    torch.set_num_threads(1)
    torch.manual_seed(267)
    model = nn.Sequential(nn.Linear(4, 8), nn.ReLU(), nn.Linear(8, 1))
    optimizer = torch.optim.SGD(model.parameters(), lr=0.05, momentum=0.9)
    features = torch.randn(64, 4)
    targets = features @ torch.tensor([[0.2], [-0.1], [0.05], [0.3]]) + 0.01
    losses = []
    for _ in range(5):
        optimizer.zero_grad()
        prediction = model(features)
        loss = torch.mean((prediction - targets) ** 2)
        loss.backward()
        optimizer.step()
        losses.append(float(loss.item()))
    print(
        {
            "torch_version": torch.__version__,
            "cuda_available": torch.cuda.is_available(),
            "losses": losses,
            "training_ok": all(value == value for value in losses),
        }
    )
    return 0 if losses[-1] < losses[0] else 1


if __name__ == "__main__":
    raise SystemExit(main())
