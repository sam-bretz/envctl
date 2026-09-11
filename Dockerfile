FROM python:3.13-alpine
WORKDIR /app
COPY calculator.py server.py ./
CMD ["python3","server.py"]
