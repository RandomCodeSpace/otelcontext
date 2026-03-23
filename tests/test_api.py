"""API endpoint smoke tests using FastAPI TestClient."""

import pytest
from httpx import AsyncClient, ASGITransport

# We need to build the app for testing
_app = None


async def get_test_app():
    global _app
    if _app is None:
        import os
        os.environ["DB_DRIVER"] = "sqlite"
        os.environ["DB_DSN"] = ":memory:"
        os.environ["APP_ENV"] = "test"
        os.environ["MCP_ENABLED"] = "true"
        os.environ["DLQ_PATH"] = "/tmp/otelcontext_test_dlq"

        from src.main import build_app
        _app = await build_app()
    return _app


@pytest.mark.asyncio
async def test_health():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/api/health")
        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "ok"


@pytest.mark.asyncio
async def test_stats():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/api/stats")
        assert resp.status_code == 200
        data = resp.json()
        assert "traces" in data
        assert "spans" in data
        assert "logs" in data


@pytest.mark.asyncio
async def test_get_traces():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/api/traces")
        assert resp.status_code == 200
        data = resp.json()
        assert "data" in data
        assert "total" in data


@pytest.mark.asyncio
async def test_get_logs():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/api/logs")
        assert resp.status_code == 200
        data = resp.json()
        assert "data" in data
        assert "total" in data


@pytest.mark.asyncio
async def test_get_services():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/api/metadata/services")
        assert resp.status_code == 200


@pytest.mark.asyncio
async def test_system_graph():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/api/system/graph")
        assert resp.status_code == 200
        data = resp.json()
        assert "system" in data
        assert "nodes" in data
        assert "edges" in data


@pytest.mark.asyncio
async def test_mcp_tools_list():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/mcp", json={
            "jsonrpc": "2.0",
            "method": "tools/list",
            "id": 1,
        })
        assert resp.status_code == 200
        data = resp.json()
        assert "result" in data
        tools = data["result"]["tools"]
        assert len(tools) == 22


@pytest.mark.asyncio
async def test_otlp_traces_json():
    app = await get_test_app()
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        payload = {
            "resourceSpans": [{
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "test-svc"}}]},
                "scopeSpans": [{
                    "spans": [{
                        "traceId": "abc123",
                        "spanId": "def456",
                        "name": "GET /api",
                        "startTimeUnixNano": "1700000000000000000",
                        "endTimeUnixNano": "1700000001000000000",
                    }]
                }]
            }]
        }
        resp = await client.post("/v1/traces", json=payload, headers={"content-type": "application/json"})
        assert resp.status_code == 200
