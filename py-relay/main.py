import os

import httpx
from openai import OpenAI

BASE_URL = os.environ.get("RELAY_BASE_URL", "https://relay.wyolet.dev/v1")
MODEL = os.environ.get("RELAY_MODEL", "gemma4:latest")

# Caddy's local CA isn't in the system trust store, so disable verify
# for local dev. Override with RELAY_VERIFY=1 when pointing at a real cert.
verify = os.environ.get("RELAY_VERIFY", "0") == "1"

client = OpenAI(
    base_url=BASE_URL,
    api_key="not-required-yet",
    http_client=httpx.Client(verify=verify),
)


def non_streaming():
    print(f"\n=== non-streaming  ({MODEL} via {BASE_URL}) ===")
    resp = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Say hi in one short sentence."}],
    )
    print(resp.choices[0].message.content)


def streaming():
    print(f"\n=== streaming  ({MODEL} via {BASE_URL}) ===")
    stream = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Count from 1 to 5, one per line."}],
        stream=True,
    )
    for chunk in stream:
        delta = chunk.choices[0].delta.content if chunk.choices else None
        if delta:
            print(delta, end="", flush=True)
    print()


def main():
    non_streaming()
    streaming()


if __name__ == "__main__":
    main()
