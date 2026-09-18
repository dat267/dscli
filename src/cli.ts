#!/usr/bin/env node
import { setupCli, main } from "./main.js";

setupCli();
const code = await main(process.argv.slice(2));
process.exitCode = code;
