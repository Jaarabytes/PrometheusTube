import { NavLink } from "@remix-run/react";
import { BellAlertIcon } from "@heroicons/react/24/outline";
import Avatar from "@mui/material/Avatar";
import Box from "@mui/material/Box";
import TextField from "@mui/material/TextField";
import Button from "@mui/material/Button";
import Checkbox from "@mui/material/Checkbox";

const style = {
  position: "absolute",
  top: "50%",
  left: "50%",
  transform: "translate(-50%, -50%)",
  width: 400,
  bgcolor: "background.paper",
  boxShadow: 24,
  p: 3,
};

export default function Login() {
  return (
    <Box
      className="rounded-lg"
      component="form"
      sx={style}
      noValidate
      autoComplete="off"
    >
      <div className="text-heading-3 flex justify-around">Welcome back!</div>
      <div className="mt-4">
        <TextField
          className="w-full"
          required
          id="outlined-required"
          label="Username"
          size="small"
        />
      </div>
      <div className="mt-4">
        <TextField
          className="w-full"
          required
          id="outlined-required"
          label="Password"
          size="small"
        />
      </div>
      <div className="flex justify-between items-center my-2 mx-0">
        <span>
          <Checkbox defaultChecked className="!ml-0 !pl-0" />
          Remember me
        </span>
        <span>
          <NavLink
            to="/forgot-password"
            className="text-single-100 underline text-primary-blue-400"
          >
            Forgot password
          </NavLink>
        </span>
      </div>
      <div>
        <Button
          color="primary"
          className="text-single-100 w-full"
          variant="contained"
        >
          Login
        </Button>
      </div>
    </Box>
  );
}