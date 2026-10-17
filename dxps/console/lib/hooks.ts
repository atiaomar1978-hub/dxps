"use client";

import { useCallback, useEffect, useState } from "react";
import { api, errorText } from "./api";

export interface Loaded<T> {
  data: T | undefined;
  error: string;
  loading: boolean;
  reload: () => void;
}

// useApi loads a resource (null path: nothing) and reloads when the path changes or reload() is called.
export function useApi<T>(path: string | null, every = 0): Loaded<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(path !== null);
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    if (path === null) return;
    let live = true;
    setLoading(true);
    api<T>(path)
      .then((d) => {
        if (!live) return;
        setData(d);
        setError("");
      })
      .catch((e) => live && setError(errorText(e)))
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, [path, tick]);

  useEffect(() => {
    if (!every) return;
    const id = setInterval(reload, every);
    return () => clearInterval(id);
  }, [every, reload]);

  return { data, error, loading, reload };
}

// useAction wraps a mutation with busy / message state.
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const run = useCallback(async (fn: () => Promise<string | void>) => {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      setMessage((await fn()) ?? "");
      return true;
    } catch (e) {
      setError(errorText(e));
      return false;
    } finally {
      setBusy(false);
    }
  }, []);
  return { busy, message, error, run };
}
