import { useEffect, useRef, useState } from "react";
import { createFileRoute, redirect, useNavigate } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { buildPlatformSession } from "@/lib/api-client";
import { completePlatformLogin, isPlatformMode, startPlatformLogin } from "@/lib/platform";
import { useAuthStore } from "@/stores/auth-store";

export const Route = createFileRoute("/auth/callback")({
  beforeLoad: () => {
    if (!isPlatformMode()) {
      throw redirect({ to: "/login", search: { redirect: undefined } });
    }
  },
  component: PlatformCallbackPage,
});

function PlatformCallbackPage() {
  const navigate = useNavigate();
  const setSession = useAuthStore((state) => state.setSession);
  const [error, setError] = useState<string | null>(null);
  // The code can be exchanged once; StrictMode runs effects twice in development.
  const started = useRef(false);

  useEffect(() => {
    if (started.current) {
      return;
    }
    started.current = true;

    void (async () => {
      try {
        const { tokens, returnTo } = await completePlatformLogin(window.location.href);
        setSession(await buildPlatformSession(tokens));
        await navigate({ to: returnTo, replace: true });
      } catch (err) {
        setError(err instanceof Error ? err.message : "Sign-in failed");
      }
    })();
  }, [navigate, setSession]);

  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      {error ? (
        <div className="text-center">
          <h1 className="text-xl font-semibold">Login gagal</h1>
          <p className="mt-2 text-sm text-muted-foreground">{error}</p>
          <Button className="mt-4" onClick={() => void startPlatformLogin()} type="button">
            Coba lagi
          </Button>
        </div>
      ) : (
        <p className="text-sm text-muted-foreground">Memproses login…</p>
      )}
    </div>
  );
}
