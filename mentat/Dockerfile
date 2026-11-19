FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY pyproject.toml ./
COPY mentat ./mentat
# Optional approved CA bundle; never disable certificate verification.
RUN --mount=type=secret,id=corporate_ca \
    if [ -s /run/secrets/corporate_ca ]; then export PIP_CERT=/run/secrets/corporate_ca; fi; \
    pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir -e .
EXPOSE 8084
CMD ["uvicorn", "mentat.app:app", "--host", "0.0.0.0", "--port", "8084"]
