FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY pyproject.toml ./
COPY truthsayer ./truthsayer
RUN pip install --no-cache-dir -e .
EXPOSE 8085
CMD ["uvicorn", "truthsayer.app:app", "--host", "0.0.0.0", "--port", "8085"]
