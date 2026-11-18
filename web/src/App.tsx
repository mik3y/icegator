import { useEffect, useState } from "react";
import { ApiError, api } from "./api";
import Console from "./Console";
import Login from "./Login";

type Auth =
  | { state: "checking" }
  | { state: "signed-out" }
  | { state: "signed-in"; username: string };

export default function App() {
  const [auth, setAuth] = useState<Auth>({ state: "checking" });

  const check = () =>
    api
      .me()
      .then((me) => setAuth({ state: "signed-in", username: me.username }))
      .catch((err) => {
        // Anything but "not signed in" is the server being unreachable.
        // The login form is still the right thing to show, since submitting
        // it will report the real problem.
        if (!(err instanceof ApiError) || err.status !== 401) console.warn(err);
        setAuth({ state: "signed-out" });
      });

  useEffect(() => {
    check();
  }, []);

  if (auth.state === "checking") return null;
  if (auth.state === "signed-out") return <Login onSignedIn={check} />;
  return (
    <Console
      username={auth.username}
      onSignedOut={() => setAuth({ state: "signed-out" })}
    />
  );
}
