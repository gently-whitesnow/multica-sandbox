"""Example-only integration with external authorization; no sandbox dependency."""

import httpx
from fastapi import HTTPException, Request
from litellm.proxy._types import UserAPIKeyAuth


async def authorize(request: Request, api_key: str) -> UserAPIKeyAuth:
    if request.url.path not in ("/v1/chat/completions", "/chat/completions"):
        raise HTTPException(403, "Route denied")
    body = await request.json()
    if body.get("model") != "demo":
        raise HTTPException(403, "Model denied")
    allowed = {
        "model", "messages", "stream", "tools", "tool_choice", "max_tokens",
        "max_completion_tokens", "temperature", "top_p", "stop", "seed",
        "frequency_penalty", "presence_penalty", "stream_options",
        "parallel_tool_calls", "reasoning_effort", "response_format",
    }
    if set(body) - allowed:
        raise HTTPException(403, "Unsupported parameter")
    try:
        async with httpx.AsyncClient(timeout=10, follow_redirects=False) as client:
            response = await client.post(
                "http://mcp:8080/authorize-inference",
                headers={"Authorization": f"Bearer {api_key}"},
            )
        if response.status_code != 200:
            raise HTTPException(403, "Identity denied")
        identity = response.json()
        if identity["model"] != body["model"]:
            raise HTTPException(403, "Model denied")
    except httpx.HTTPError as exc:
        raise HTTPException(403, "Authorization unavailable") from exc
    return UserAPIKeyAuth(
        user_id=identity["agent"],
        team_id=identity["workspace"],
        models=["demo"],
        rpm_limit=20,
        tpm_limit=30000,
    )
