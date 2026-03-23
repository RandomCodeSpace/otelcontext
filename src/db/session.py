"""Async SQLAlchemy engine and session factory."""

from __future__ import annotations

from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession, async_sessionmaker, create_async_engine

from src.config import Config
from src.db.models import Base

_engine: AsyncEngine | None = None
_session_factory: async_sessionmaker[AsyncSession] | None = None


def _build_url(cfg: Config) -> str:
    """Build the async database URL from config."""
    driver = cfg.db_driver.lower()
    if driver == "sqlite":
        dsn = cfg.db_dsn or "otelcontext.db"
        return f"sqlite+aiosqlite:///{dsn}"
    if driver in ("postgres", "postgresql"):
        dsn = cfg.db_dsn or "postgresql+asyncpg://localhost/otelcontext"
        if dsn.startswith("postgresql://"):
            dsn = dsn.replace("postgresql://", "postgresql+asyncpg://", 1)
        return dsn
    # Fallback
    return f"sqlite+aiosqlite:///otelcontext.db"


async def init_db(cfg: Config) -> AsyncEngine:
    """Initialize the async engine, create tables, and return the engine."""
    global _engine, _session_factory
    url = _build_url(cfg)
    _engine = create_async_engine(url, echo=False, pool_pre_ping=True)
    _session_factory = async_sessionmaker(_engine, expire_on_commit=False)
    async with _engine.begin() as conn:
        await conn.run_sync(Base.metadata.create_all)
    return _engine


def get_session_factory() -> async_sessionmaker[AsyncSession]:
    """Return the session factory. Must call init_db first."""
    assert _session_factory is not None, "Database not initialized. Call init_db() first."
    return _session_factory


def get_session() -> AsyncSession:
    """Create a new async session."""
    return get_session_factory()()


async def close_db() -> None:
    """Dispose the engine."""
    global _engine, _session_factory
    if _engine is not None:
        await _engine.dispose()
        _engine = None
        _session_factory = None
