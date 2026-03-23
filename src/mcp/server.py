"""MCP Server: JSON-RPC 2.0 over HTTP POST + SSE GET.

Hand-rolled implementation (~200 LOC) for maximum compatibility.
Implements 22 tools matching the Go version.
"""

from __future__ import annotations

import json
import logging
from datetime import datetime, timedelta, timezone
from typing import Any

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, StreamingResponse

from src.mcp.tools import execute_tool, list_tools

logger = logging.getLogger(__name__)

router = APIRouter()


@router.post("")
@router.post("/")
async def mcp_jsonrpc(request: Request):
    """Handle JSON-RPC 2.0 requests."""
    try:
        body = await request.json()
    except Exception:
        return JSONResponse(
            {"jsonrpc": "2.0", "error": {"code": -32700, "message": "Parse error"}, "id": None},
            status_code=400,
        )

    method = body.get("method", "")
    params = body.get("params", {})
    req_id = body.get("id")

    if method == "initialize":
        return JSONResponse({
            "jsonrpc": "2.0",
            "result": {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "otelcontext", "version": "6.0.0"},
            },
            "id": req_id,
        })

    if method == "tools/list":
        return JSONResponse({
            "jsonrpc": "2.0",
            "result": {"tools": list_tools()},
            "id": req_id,
        })

    if method == "tools/call":
        tool_name = params.get("name", "")
        arguments = params.get("arguments", {})
        try:
            result = await execute_tool(request.app, tool_name, arguments)
            return JSONResponse({
                "jsonrpc": "2.0",
                "result": {
                    "content": [{"type": "text", "text": json.dumps(result, default=str)}],
                    "isError": False,
                },
                "id": req_id,
            })
        except Exception as e:
            logger.error("MCP tool error: %s: %s", tool_name, e)
            return JSONResponse({
                "jsonrpc": "2.0",
                "result": {
                    "content": [{"type": "text", "text": str(e)}],
                    "isError": True,
                },
                "id": req_id,
            })

    if method == "notifications/initialized":
        # Client notification, no response needed for notifications but we respond for HTTP
        return JSONResponse({"jsonrpc": "2.0", "result": {}, "id": req_id})

    return JSONResponse({
        "jsonrpc": "2.0",
        "error": {"code": -32601, "message": f"Method not found: {method}"},
        "id": req_id,
    })


@router.get("")
@router.get("/")
async def mcp_sse(request: Request):
    """SSE endpoint for server-sent events (stub)."""
    async def event_stream():
        yield f"data: {json.dumps({'type': 'endpoint', 'url': str(request.url)})}\n\n"

    return StreamingResponse(event_stream(), media_type="text/event-stream")
