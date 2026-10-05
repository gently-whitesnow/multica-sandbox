"""Fixture uses LiteLLM's external authorization hook; no provider credentials in agents."""
import httpx
from fastapi import HTTPException, Request
from litellm.proxy._types import UserAPIKeyAuth

async def authorize(request: Request, api_key: str) -> UserAPIKeyAuth:
    if request.url.path not in ("/v1/chat/completions", "/chat/completions"):
        raise HTTPException(403, "Route denied")
    body = await request.json()
    allowed = {
        "model", "messages", "stream", "tools", "tool_choice", "max_tokens",
        "max_completion_tokens", "temperature", "top_p", "stop", "seed",
        "frequency_penalty", "presence_penalty", "stream_options",
        "parallel_tool_calls", "reasoning_effort", "response_format",
    }
    if set(body) - allowed:
        raise HTTPException(403, "Unsupported parameter")
    async with httpx.AsyncClient(timeout=5, follow_redirects=False) as client:
        response = await client.post(
            "http://gateway:8080/authorize-inference",
            headers={"Authorization": f"Bearer {api_key}"},
        )
    if response.status_code != 200:
        raise HTTPException(403, "Identity denied")
    binding = response.json()
    if body.get("model") not in binding["models"]:
        raise HTTPException(403, "Model denied")
    return UserAPIKeyAuth(
        user_id=binding["agent"], team_id=binding["workspace"],
        models=binding["models"], rpm_limit=120, tpm_limit=1000000,
    )
