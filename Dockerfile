FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /opt/webhook-lab
COPY app ./app
COPY receiver ./receiver
COPY loadgen ./loadgen

EXPOSE 8080
CMD ["python", "-m", "app.main"]

