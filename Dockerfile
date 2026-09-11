FROM python@sha256:7415fbc3c9e4979cc717d92377ab2bc7b2b4a2af1ac03cc52b5f3f88efedaf3a
WORKDIR /app
COPY calculator.py server.py ./
CMD ["python3","server.py"]
