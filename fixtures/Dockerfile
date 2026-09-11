FROM python@sha256:7415fbc3c9e4979cc717d92377ab2bc7b2b4a2af1ac03cc52b5f3f88efedaf3a
RUN adduser -D -u 10001 fixture && mkdir /data && chown fixture /data
COPY http_fixture.py /opt/envctl/http_fixture.py
USER fixture
CMD ["python3","/opt/envctl/http_fixture.py","serve"]
